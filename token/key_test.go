package token

import (
	"cmp"
	"encoding/base64"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
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
		wantPause   []Action
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
			wantPrefix: "/acme/app/",
		},
		// A bare prefix reads as the directory it names.
		"a bare prefix": {
			raw:        `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"` + rfc8037X + `","prefix":"acme/app"}]}`,
			wantKid:    rfc8037Kid,
			wantPrefix: "/acme/app/",
		},
		"publishing paused": {
			raw:       `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"` + rfc8037X + `","pause":["publish"]}]}`,
			wantKid:   rfc8037Kid,
			wantPause: []Action{ActionPublish},
		},
		"a pause listed twice is one": {
			raw:       `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"` + rfc8037X + `","pause":["publish","publish"]}]}`,
			wantKid:   rfc8037Kid,
			wantPause: []Action{ActionPublish},
		},
		"an empty pause is none": {
			raw:     `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"` + rfc8037X + `","pause":[]}]}`,
			wantKid: rfc8037Kid,
		},
		"pause names something else": {
			raw:         `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"` + rfc8037X + `","pause":["subscribe"]}]}`,
			wantErrText: "only",
		},
		"pause misspelled": {
			raw:         `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"` + rfc8037X + `","pause":["publsh"]}]}`,
			wantErrText: "only",
		},
		"an unknown member is ignored": {
			raw:     `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"` + rfc8037X + `","publish":false}]}`,
			wantKid: rfc8037Kid,
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
		// A second entry could give the key a wider prefix; neither may win.
		"a key listed twice": {
			raw: `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"` + rfc8037X + `","prefix":"acme"},` +
				`{"kty":"OKP","crv":"Ed25519","x":"` + rfc8037X + `"}]}`,
			wantErrText: "listed twice",
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
				assert.Equal(t, cmp.Or(tt.wantPrefix, "/"), keys[tt.wantKid].Prefix, "no prefix confines nothing")
				assert.Equal(t, tt.wantPause, keys[tt.wantKid].Pause)
			}
		})
	}
}

// A key set a server writes with MarshalKeySet reads back with each key's
// prefix and pause.
func TestMarshalKeySet_RoundTrip(t *testing.T) {
	free, err := GenerateKey("acme/app")
	require.NoError(t, err)
	limited, err := GenerateKey("acme/app")
	require.NoError(t, err)
	paused := limited.Public()
	paused.Pause = []Action{ActionPublish}

	raw, err := MarshalKeySet(free.Public(), paused)
	require.NoError(t, err)
	keys, err := ParseKeySet(raw)

	require.NoError(t, err)
	assert.Equal(t, free.Public(), keys[free.ID])
	assert.Equal(t, paused, keys[limited.ID])
	assert.False(t, keys[free.ID].Pauses(ActionPublish))
	assert.True(t, keys[limited.ID].Pauses(ActionPublish))
	assert.NotContains(t, string(raw), `"pause"`+`:null`, "an unpaused key carries no pause")
}

func TestMarshalKeySet_RefusesAnotherPause(t *testing.T) {
	key, err := GenerateKey("acme/app")
	require.NoError(t, err)
	public := key.Public()
	public.Pause = []Action{ActionSubscribe}

	_, err = MarshalKeySet(public)

	assert.ErrorContains(t, err, "only", "a set a verifier would refuse is not written")
}

// What keygen writes, the relay and the app read back: the public key
// set and the private signing key agree on the kid and prefix.
func TestSigningKey_MarshalJWK_RoundTrip(t *testing.T) {
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

	assert.Equal(t, "/acme/app/", key.Prefix)
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

// Holds is what the relay re-checks a live session's tickets with, so it
// must agree with Sign: the key's prefix and its /.hls/ mirror, at a "/"
// boundary, and nothing else. /.abr/ is not mirrored: an ABR opt-in is paid
// for, and takes a key of its own.
func TestKey_Holds(t *testing.T) {
	tests := map[string]struct {
		prefix string
		root   string
		want   bool
	}{
		"the empty root":                  {prefix: "/acme/", root: "", want: true},
		"any root for an unconfined key":  {prefix: "/", root: "/other/x/", want: true},
		"the prefix itself":               {prefix: "/acme/", root: "/acme/", want: true},
		"beneath the prefix":              {prefix: "/acme/", root: "/acme/room/42/", want: true},
		"beneath the HLS mirror":          {prefix: "/acme/", root: "/.hls/acme/room/42/", want: true},
		"the HLS mirror itself":           {prefix: "/acme/", root: "/.hls/acme/", want: true},
		"a sibling sharing a name prefix": {prefix: "/acme/", root: "/acme2/", want: false},
		"a sibling of the mirror":         {prefix: "/acme/", root: "/.hls/acme2/", want: false},
		"above the prefix":                {prefix: "/acme/", root: "/", want: false},
		"above the mirror":                {prefix: "/acme/", root: "/.hls/", want: false},
		"another tenant's mirror":         {prefix: "/acme/", root: "/.hls/other/", want: false},
		"a mirror of the mirror":          {prefix: "/acme/", root: "/.hls/.hls/acme/", want: false},
		"not the ABR directory":           {prefix: "/acme/", root: "/.abr/acme/room/42/", want: false},
		"not the ABR directory itself":    {prefix: "/acme/", root: "/.abr/acme/", want: false},
		"an ABR key, beneath its prefix":  {prefix: "/.abr/acme/", root: "/.abr/acme/room/42/", want: true},
		"an ABR key, not the tenant":      {prefix: "/.abr/acme/", root: "/acme/room/42/", want: false},
		"an ABR key, not another tenant":  {prefix: "/.abr/acme/", root: "/.abr/other/", want: false},
		"an ABR key, not mirrored to HLS": {prefix: "/.abr/acme/", root: "/.hls/acme/", want: false},
		"a directory that is no opt-in":   {prefix: "/acme/", root: "/.other/acme/", want: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, Key{Prefix: tt.prefix}.Holds(tt.root))
		})
	}
}

func TestKey_HoldsOptIns(t *testing.T) {
	tests := map[string]struct {
		optIns []OptIn
		root   string
		want   bool
	}{
		"default: HLS":              {optIns: nil, root: "/.hls/acme/x/", want: true},
		"default: no ABR":           {optIns: nil, root: "/.abr/acme/x/", want: false},
		"ABR held":                  {optIns: []OptIn{OptInABR}, root: "/.abr/acme/x/", want: true},
		"ABR held, not another's":   {optIns: []OptIn{OptInABR}, root: "/.abr/other/x/", want: false},
		"ABR held, HLS not":         {optIns: []OptIn{OptInABR}, root: "/.hls/acme/x/", want: false},
		"none held":                 {optIns: []OptIn{}, root: "/.hls/acme/x/", want: false},
		"none held, own prefix":     {optIns: []OptIn{}, root: "/acme/x/", want: true},
		"unknown opt-in holds none": {optIns: []OptIn{"dash"}, root: "/.dash/acme/x/", want: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, Key{Prefix: "/acme/", OptIns: tt.optIns}.Holds(tt.root))
		})
	}
}

func TestKeySet_OptIns(t *testing.T) {
	key, err := GenerateKey("/acme/")
	require.NoError(t, err)
	pub := key.Public()
	jwkWith := func(member string) []byte {
		j := pub.jwk()
		raw, err := json.Marshal(j)
		require.NoError(t, err)
		obj := strings.TrimSuffix(string(raw), "}")
		if member != "" {
			obj += "," + member
		}
		return []byte(`{"keys":[` + obj + `}]}`)
	}
	tests := map[string]struct {
		member  string
		want    []OptIn
		wantErr bool
	}{
		"absent is the default":      {member: "", want: nil},
		"empty holds none":           {member: `"optIns":[]`, want: []OptIn{}},
		"listed, in canonical order": {member: `"optIns":["abr","hls","abr"]`, want: []OptIn{OptInHLS, OptInABR}},
		"an unknown opt-in":          {member: `"optIns":["dash"]`, wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			keys, err := ParseKeySet(jwkWith(tt.member))
			if tt.wantErr {
				assert.ErrorContains(t, err, "optIns")
				return
			}
			require.NoError(t, err)
			got := keys[pub.ID].OptIns
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.want == nil, got == nil, "absent and empty stay apart")

			// And they survive a round trip through MarshalKeySet.
			raw, err := MarshalKeySet(keys[pub.ID])
			require.NoError(t, err)
			again, err := ParseKeySet(raw)
			require.NoError(t, err)
			assert.Equal(t, got, again[pub.ID].OptIns)
			assert.Equal(t, got == nil, again[pub.ID].OptIns == nil)
		})
	}
	_, err = MarshalKeySet(Key{ID: pub.ID, Public: pub.Public, Prefix: "/acme/", OptIns: []OptIn{"dash"}})
	assert.Error(t, err, "an unknown opt-in is not written")
}
