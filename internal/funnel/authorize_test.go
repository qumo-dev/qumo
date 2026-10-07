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

	tests := map[string]struct {
		header      string
		wantErr     bool
		wantUnauthn bool
	}{
		"a credential for the broadcast": {header: "Bearer " + sign(t, key, token.Grant{Publish: "/room/123/comments"})},
		"a credential for the room":      {header: "Bearer " + sign(t, key, token.Grant{Publish: "/room/123"})},
		"a credential for every room":    {header: "Bearer " + sign(t, key, token.Grant{Publish: "/room"})},
		"lower-case scheme":              {header: "bearer " + sign(t, key, token.Grant{Publish: "/room/123"})},
		"a subscribe-only credential":    {header: "Bearer " + sign(t, key, token.Grant{Subscribe: "/room/123"}), wantErr: true},
		"no credential":                  {wantErr: true, wantUnauthn: true},
		"not a bearer credential":        {header: "Basic YWxpY2U6c2VjcmV0", wantErr: true, wantUnauthn: true},
		"signed by an unknown key":       {header: "Bearer " + sign(t, other, token.Grant{Publish: "/room/123"}), wantErr: true, wantUnauthn: true},
		"not a credential at all":        {header: "Bearer hello", wantErr: true, wantUnauthn: true},
		"a credential for a sibling":     {header: "Bearer " + sign(t, key, token.Grant{Publish: "/room/1234"}), wantErr: true},
		"a credential for a sub-path":    {header: "Bearer " + sign(t, key, token.Grant{Publish: "/room/123/comments/alice"}), wantErr: true},
		"a credential for another room":  {header: "Bearer " + sign(t, key, token.Grant{Publish: "/room/9"}), wantErr: true},
	}
	authorize := authorizer(v)
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/announce", nil)
			if tt.header != "" {
				r.Header.Set("Authorization", tt.header)
			}

			err := authorize(r, chat)

			if !tt.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tt.wantUnauthn, errors.Is(err, ingest.ErrUnauthenticated))
		})
	}
}

func TestAuthorizer_NoVerifierChecksNothing(t *testing.T) {
	assert.Nil(t, authorizer(nil))
}
