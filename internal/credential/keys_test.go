package credential

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Ed25519 public key of RFC 8037 appendix A.2, and its RFC 7638
// thumbprint from appendix A.3.
const (
	rfc8037X          = "11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo"
	rfc8037Thumbprint = "kPrK_qmxVWaYVA9wwBF6Iuo3vVzz7TxHCTwXBygrS4k"
)

func TestThumbprint_RFC8037(t *testing.T) {
	assert.Equal(t, rfc8037Thumbprint, thumbprint(rfc8037X))
}

func TestParseKeys(t *testing.T) {
	x, err := base64.RawURLEncoding.DecodeString(rfc8037X)
	require.NoError(t, err)
	want := StaticKeys{rfc8037Thumbprint: ed25519.PublicKey(x)}
	set := func(keys ...map[string]string) []byte {
		raw, err := json.Marshal(map[string]any{"keys": keys})
		require.NoError(t, err)
		return raw
	}
	okp := func(kid string) map[string]string {
		k := map[string]string{"kty": "OKP", "crv": "Ed25519", "x": rfc8037X}
		if kid != "" {
			k["kid"] = kid
		}
		return k
	}

	tests := map[string]struct {
		raw     []byte
		want    StaticKeys
		wantErr bool
	}{
		"kid given as the thumbprint": {raw: set(okp(rfc8037Thumbprint)), want: want},
		"kid derived when absent":     {raw: set(okp("")), want: want},
		"kid not the thumbprint":      {raw: set(okp("my-key")), wantErr: true},
		"not Ed25519":                 {raw: set(map[string]string{"kty": "EC", "crv": "P-256", "x": rfc8037X}), wantErr: true},
		"x too short":                 {raw: set(map[string]string{"kty": "OKP", "crv": "Ed25519", "x": "AAAA"}), wantErr: true},
		"no keys":                     {raw: set(), wantErr: true},
		"not JSON":                    {raw: []byte("{"), wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := parseKeys(tt.raw)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLoadKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	raw := []byte(`{"keys":[{"kty":"OKP","crv":"Ed25519","x":"` + rfc8037X + `"}]}`)
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	keys, err := LoadKeys(path)

	require.NoError(t, err)
	_, err = keys.Key(t.Context(), rfc8037Thumbprint)
	assert.NoError(t, err)
	_, err = keys.Key(t.Context(), "unknown")
	assert.ErrorIs(t, err, errUnknownKey)
}

func TestLoadKeys_MissingFile(t *testing.T) {
	_, err := LoadKeys(filepath.Join(t.TempDir(), "absent.json"))

	assert.ErrorIs(t, err, os.ErrNotExist)
}
