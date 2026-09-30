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
		audience     string // QUMO_RELAY_AUDIENCE (removed)
		issuer       string // QUMO_CREDENTIAL_ISSUER (removed)
		wantErr      error
		wantErrText  string
		wantEnabled  bool
		wantMeter    bool
	}{
		"nothing configured (open relay)": {},
		"signing keys": {
			keysFile:    keysFile,
			wantEnabled: true,
		},
		"signing keys and usage reporting": {
			keysFile:     keysFile,
			controlPlane: controlPlane,
			wantEnabled:  true,
			wantMeter:    true,
		},
		"usage reporting without keys would run open": {
			controlPlane: controlPlane,
			wantErr:      errUsageWithoutKeys,
		},
		"unreadable keys file": {
			keysFile:    filepath.Join(t.TempDir(), "absent.json"),
			wantErrText: "QUMO_SIGNING_KEYS_FILE",
		},
		"removed audience setting": {
			keysFile:    keysFile,
			audience:    "qumo-relay-dev",
			wantErrText: "QUMO_RELAY_AUDIENCE",
		},
		"removed issuer setting": {
			issuer:      "https://api.example.com",
			wantErrText: "QUMO_CREDENTIAL_ISSUER",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("QUMO_SIGNING_KEYS_FILE", tt.keysFile)
			t.Setenv("QUMO_CREDENTIAL_URL", tt.controlPlane)
			t.Setenv("QUMO_RELAY_AUDIENCE", tt.audience)
			t.Setenv("QUMO_CREDENTIAL_ISSUER", tt.issuer)

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
			assert.Equal(t, tt.wantEnabled, auth.enabled())
			assert.Equal(t, tt.wantMeter, auth.meter != nil)
			assert.Equal(t, tt.wantMeter, auth.usage != nil)
		})
	}
}
