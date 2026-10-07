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
	alice := ingest.Announcement{BroadcastPath: "/room/123", TrackName: "chat", Name: "alice"}

	tests := map[string]struct {
		header      string
		wantErr     bool
		wantUnauthn bool
	}{
		"alice's credential":           {header: "Bearer " + sign(t, key, token.Grant{Publish: "/room/123/alice"})},
		"a credential for the room":    {header: "Bearer " + sign(t, key, token.Grant{Publish: "/room/123"})},
		"lower-case scheme":            {header: "bearer " + sign(t, key, token.Grant{Publish: "/room/123/alice"})},
		"bob's credential":             {header: "Bearer " + sign(t, key, token.Grant{Publish: "/room/123/bob"}), wantErr: true},
		"a subscribe-only credential":  {header: "Bearer " + sign(t, key, token.Grant{Subscribe: "/room/123"}), wantErr: true},
		"no credential":                {wantErr: true, wantUnauthn: true},
		"not a bearer credential":      {header: "Basic YWxpY2U6c2VjcmV0", wantErr: true, wantUnauthn: true},
		"signed by an unknown key":     {header: "Bearer " + sign(t, other, token.Grant{Publish: "/room/123/alice"}), wantErr: true, wantUnauthn: true},
		"not a credential at all":      {header: "Bearer hello", wantErr: true, wantUnauthn: true},
		"a credential for a sibling":   {header: "Bearer " + sign(t, key, token.Grant{Publish: "/room/1234"}), wantErr: true},
		"a credential for a sub-path":  {header: "Bearer " + sign(t, key, token.Grant{Publish: "/room/123/alice/phone"}), wantErr: true},
		"a credential for another one": {header: "Bearer " + sign(t, key, token.Grant{Publish: "/room/9/alice"}), wantErr: true},
	}
	authorize := authorizer(v)
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/announce", nil)
			if tt.header != "" {
				r.Header.Set("Authorization", tt.header)
			}

			err := authorize(r, alice)

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
