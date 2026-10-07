package funnel

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/okdaichi/qumo-ledger/ingest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/token"
)

// newVerifier returns a Verifier trusting a fresh key, and the key.
func newVerifier(tb testing.TB) (*auth.Verifier, token.SigningKey) {
	tb.Helper()
	key, err := token.GenerateKey("")
	require.NoError(tb, err)
	set, err := token.MarshalKeySet(key.Public())
	require.NoError(tb, err)
	keys := filepath.Join(tb.TempDir(), "keys.json")
	require.NoError(tb, os.WriteFile(keys, set, 0o600))
	v, err := auth.NewVerifier(auth.VerifierConfig{Keys: keys})
	require.NoError(tb, err)
	return v, key
}

func sign(tb testing.TB, key token.SigningKey, g token.Grant) string {
	tb.Helper()
	credential, err := token.Sign(key, g, time.Minute)
	require.NoError(tb, err)
	return credential
}

func TestAuthorizer(t *testing.T) {
	v, key := newVerifier(t)
	other, err := token.GenerateKey("")
	require.NoError(t, err)
	chat := ingest.Track{BroadcastPath: "/room/123/comments", TrackName: "comments"}

	bearerOf := func(g token.Grant) string { return "Bearer " + sign(t, key, g) }

	tests := map[string]struct {
		header      string
		access      ingest.Access
		wantSender  string
		wantErr     bool
		wantUnauthn bool
	}{
		"publishing at the broadcast":        {header: bearerOf(token.Grant{Publish: "/room/123/comments"})},
		"publishing at the room":             {header: bearerOf(token.Grant{Publish: "/room/123"})},
		"publishing at every room":           {header: bearerOf(token.Grant{Publish: "/room"})},
		"publishing as a sender":             {header: bearerOf(token.Grant{Publish: "/room/123/comments/user-42"}), wantSender: "user-42"},
		"lower-case scheme":                  {header: "bearer " + sign(t, key, token.Grant{Publish: "/room/123"})},
		"publishing two segments beneath":    {header: bearerOf(token.Grant{Publish: "/room/123/comments/user-42/phone"}), wantErr: true},
		"publishing as a sender elsewhere":   {header: bearerOf(token.Grant{Publish: "/room/9/comments/user-42"}), wantErr: true},
		"a subscribe-only credential":        {header: bearerOf(token.Grant{Subscribe: "/room/123"}), wantErr: true},
		"no credential":                      {wantErr: true, wantUnauthn: true},
		"not a bearer credential":            {header: "Basic YWxpY2U6c2VjcmV0", wantErr: true, wantUnauthn: true},
		"signed by an unknown key":           {header: "Bearer " + sign(t, other, token.Grant{Publish: "/room/123"}), wantErr: true, wantUnauthn: true},
		"not a credential at all":            {header: "Bearer hello", wantErr: true, wantUnauthn: true},
		"publishing at a sibling":            {header: bearerOf(token.Grant{Publish: "/room/1234"}), wantErr: true},
		"publishing at another room":         {header: bearerOf(token.Grant{Publish: "/room/9"}), wantErr: true},
		"reading with a subscriber's":        {header: bearerOf(token.Grant{Subscribe: "/room/123"}), access: ingest.Read},
		"reading with a publisher's":         {header: bearerOf(token.Grant{Publish: "/room/123"}), access: ingest.Read, wantErr: true},
		"reading another room":               {header: bearerOf(token.Grant{Subscribe: "/room/9"}), access: ingest.Read, wantErr: true},
		"reading with a sender's credential": {header: bearerOf(token.Grant{Publish: "/room/123/comments/user-42"}), access: ingest.Read, wantErr: true},
	}
	authorize := authorizer(v)
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/tracks/room/123/comments/comments", nil)
			if tt.header != "" {
				r.Header.Set("Authorization", tt.header)
			}

			sender, err := authorize(r, chat, tt.access)

			if !tt.wantErr {
				require.NoError(t, err)
				assert.Equal(t, tt.wantSender, sender)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tt.wantUnauthn, errors.Is(err, ingest.ErrUnauthenticated))
		})
	}
}

func TestSenderOf(t *testing.T) {
	tests := map[string]struct {
		bases      []string
		wantSender string
		wantOK     bool
	}{
		"one segment beneath":  {bases: []string{"room/123/comments/user-42"}, wantSender: "user-42", wantOK: true},
		"the first that names": {bases: []string{"room/9", "room/123/comments/user-7"}, wantSender: "user-7", wantOK: true},
		"the broadcast itself": {bases: []string{"room/123/comments"}},
		"two segments beneath": {bases: []string{"room/123/comments/a/b"}},
		"beneath another":      {bases: []string{"room/123/other/user-42"}},
		"everything":           {bases: []string{""}},
		"no grant":             {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			sender, ok := senderOf(tt.bases, "/room/123/comments")

			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantSender, sender)
		})
	}
}

func TestAuthorizer_NoVerifierChecksNothing(t *testing.T) {
	assert.Nil(t, authorizer(nil))
}
