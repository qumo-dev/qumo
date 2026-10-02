package relay

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeKeysFile writes a JWK Set holding one fresh Ed25519 public key and
// returns its path.
func writeKeysFile(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	x := base64.RawURLEncoding.EncodeToString(pub)
	path := filepath.Join(t.TempDir(), "keys.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"keys":[{"kty":"OKP","crv":"Ed25519","x":"`+x+`"}]}`), 0o600))
	return path
}

func TestNewCredentialAuth(t *testing.T) {
	keysFile := writeKeysFile(t)
	const controlPlane = "https://cp.example.com"
	// Each field sets the environment variable of the same meaning; empty
	// leaves it unset.
	tests := map[string]struct {
		keysFile     string // QUMO_SIGNING_KEYS_FILE
		controlPlane string // QUMO_CREDENTIAL_URL
		relayToken   bool   // QUMO_RELAY_TOKEN set
		wantErr      error
		wantErrText  string
		wantStatic   bool
		wantManaged  bool
	}{
		"nothing configured (open relay)": {},
		"static keys (self-hosted)": {
			keysFile:   keysFile,
			wantStatic: true,
		},
		"control plane (managed)": {
			controlPlane: controlPlane,
			relayToken:   true,
			wantManaged:  true,
		},
		"managed without the relay token": {
			controlPlane: controlPlane,
			wantErr:      errNoRelayToken,
		},
		"both trust sources": {
			keysFile:     keysFile,
			controlPlane: controlPlane,
			relayToken:   true,
			wantErr:      errBothTrustSources,
		},
		"unreadable keys file": {
			keysFile:    filepath.Join(t.TempDir(), "absent.json"),
			wantErrText: "QUMO_SIGNING_KEYS_FILE",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("QUMO_SIGNING_KEYS_FILE", tt.keysFile)
			t.Setenv("QUMO_CREDENTIAL_URL", tt.controlPlane)
			t.Setenv("QUMO_RELAY_TOKEN", "")
			if tt.relayToken {
				t.Setenv("QUMO_RELAY_TOKEN", "x")
			}

			auth, err := newCredentialAuth()

			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			if tt.wantErrText != "" {
				assert.ErrorContains(t, err, tt.wantErrText)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantStatic || tt.wantManaged, auth.enabled())
			assert.Equal(t, tt.wantStatic, auth.keys != nil, "static keys")
			assert.Equal(t, tt.wantManaged, auth.trust != nil && auth.poller != nil, "trust snapshot")
			assert.Equal(t, tt.wantManaged, auth.meter != nil && auth.usage != nil, "usage reporting")
		})
	}
}
