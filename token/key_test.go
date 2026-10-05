package token

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

func TestParseKeySet(t *testing.T) {
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
		"a private key in the set": {
			raw:         `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"` + rfc8037X + `","d":"nWGxne_9WmC6hEr0kuwsxERJxWl7MmkZcDusAxyuf2A"}]}`,
			wantErrText: "public keys only",
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
			keys, err := ParseKeySet([]byte(tt.raw))

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

// What keygen writes, the auth server and the app read back: the public key
// set and the private signing key agree on the kid and prefix.
func TestKeyFiles_RoundTrip(t *testing.T) {
	key, err := GenerateKey("/acme/app/")
	require.NoError(t, err)
	dir := t.TempDir()
	set, err := MarshalKeySet(key.Public())
	require.NoError(t, err)
	priv, err := key.MarshalJWK()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keys.json"), set, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "signing-key.jwk"), priv, 0o600))

	keys, err := LoadKeySet(filepath.Join(dir, "keys.json"))
	require.NoError(t, err)
	signing, err := LoadSigningKey(filepath.Join(dir, "signing-key.jwk"))
	require.NoError(t, err)

	assert.Equal(t, "acme/app", key.Prefix)
	assert.Equal(t, key.Public(), keys[key.ID])
	assert.Equal(t, key, signing)
}

func TestParseSigningKey_Refusals(t *testing.T) {
	tests := map[string]struct {
		raw         string
		wantErrText string
	}{
		"a public key only": {raw: `{"kty":"OKP","crv":"Ed25519","x":"` + rfc8037X + `"}`, wantErrText: "private key"},
		"not Ed25519":       {raw: `{"kty":"EC","crv":"P-256","d":"AAAA"}`, wantErrText: "Ed25519"},
		"kid not the thumbprint": {
			raw:         `{"kty":"OKP","crv":"Ed25519","d":"nWGxne_9WmC6hEr0kuwsxERJxWl7MmkZcDusAxyuf2A","kid":"other"}`,
			wantErrText: "thumbprint",
		},
		"not JSON": {raw: `jwk`, wantErrText: "decode"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSigningKey([]byte(tt.raw))

			assert.ErrorContains(t, err, tt.wantErrText)
		})
	}
}

// RFC 8037 Appendix A.1's private key d derives the public key x of A.2, and
// so the A.3 kid.
func TestParseSigningKey_RFC8037Vector(t *testing.T) {
	key, err := ParseSigningKey([]byte(`{"kty":"OKP","crv":"Ed25519","d":"nWGxne_9WmC6hEr0kuwsxERJxWl7MmkZcDusAxyuf2A"}`))

	require.NoError(t, err)
	assert.Equal(t, rfc8037Kid, key.ID)
	assert.Equal(t, rfc8037X, base64.RawURLEncoding.EncodeToString(key.Public().Public))
}

func TestLoadKeySet_MissingFile(t *testing.T) {
	_, err := LoadKeySet(filepath.Join(t.TempDir(), "absent.json"))

	assert.ErrorContains(t, err, "read")
}
