package authserver

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rfc8037X and rfc8037Kid are the Ed25519 key and its thumbprint from RFC 8037
// Appendix A.3.
const (
	rfc8037X   = "11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo"
	rfc8037Kid = "kPrK_qmxVWaYVA9wwBF6Iuo3vVzz7TxHCTwXBygrS4k"
)

func TestThumbprint_RFC8037Vector(t *testing.T) {
	x, err := base64.RawURLEncoding.DecodeString(rfc8037X)
	require.NoError(t, err)

	got := thumbprint(x)

	assert.Equal(t, rfc8037Kid, got)
}

func TestParseKeys(t *testing.T) {
	tests := map[string]struct {
		raw         string
		wantKid     string
		wantPrefix  string
		wantErr     error
		wantErrText string
	}{
		"kid derived": {
			raw:     `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"` + rfc8037X + `"}]}`,
			wantKid: rfc8037Kid,
		},
		"kid matching the thumbprint": {
			raw:     `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"` + rfc8037X + `","kid":"` + rfc8037Kid + `"}]}`,
			wantKid: rfc8037Kid,
		},
		// The last character's spare bits differ ("p" for "o"), which decodes
		// to the same key; the kid must not depend on how x was written.
		"non-canonical x gets the canonical kid": {
			raw:     `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURp"}]}`,
			wantKid: rfc8037Kid,
		},
		"prefix normalized": {
			raw:        `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"` + rfc8037X + `","prefix":"/acme//app/"}]}`,
			wantKid:    rfc8037Kid,
			wantPrefix: "acme/app",
		},
		"kid not the thumbprint": {
			raw:         `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"` + rfc8037X + `","kid":"other"}]}`,
			wantErrText: "thumbprint",
		},
		"not Ed25519": {
			raw:         `{"keys":[{"kty":"EC","crv":"P-256","x":"` + rfc8037X + `"}]}`,
			wantErrText: "Ed25519",
		},
		"x too short": {
			raw:         `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"AAAA"}]}`,
			wantErrText: "Ed25519 public key",
		},
		"prefix escaping with ..": {
			raw:         `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"` + rfc8037X + `","prefix":"acme/../other"}]}`,
			wantErrText: "prefix",
		},
		"no keys":  {raw: `{"keys":[]}`, wantErr: errNoKeys},
		"not JSON": {raw: `keys`, wantErrText: "decode"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			keys, err := parseKeys([]byte(tt.raw))

			switch {
			case tt.wantErr != nil:
				assert.ErrorIs(t, err, tt.wantErr)
			case tt.wantErrText != "":
				assert.ErrorContains(t, err, tt.wantErrText)
			default:
				require.NoError(t, err)
				require.Contains(t, keys, tt.wantKid)
				assert.Equal(t, tt.wantPrefix, keys[tt.wantKid].Prefix)
			}
		})
	}
}

func TestLoadKeys_ReadsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"keys":[{"kty":"OKP","crv":"Ed25519","x":"`+rfc8037X+`"}]}`), 0o600))

	keys, err := LoadKeys(path)

	require.NoError(t, err)
	assert.Contains(t, keys, rfc8037Kid)
}

func TestLoadKeys_MissingFile(t *testing.T) {
	_, err := LoadKeys(filepath.Join(t.TempDir(), "absent.json"))

	assert.ErrorContains(t, err, "read keys")
}
