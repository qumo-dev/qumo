package token

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerify_AcceptsAValidToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signer := &rawSigner{}

	c, err := Verify(signer.sign(t, validClaims(now)), signer.keys(t), now)

	require.NoError(t, err)
	assert.Equal(t, signer.trusted(t).ID, c.Key.ID)
	assert.Equal(t, Grant{Scopes: []Scope{pubScope("acme/app/alice"), subScope("acme/app")}}, c.Grant)
	assert.Equal(t, now.Add(10*time.Minute), c.ExpiresAt)
	assert.Equal(t, "j-1", c.ID)
}

func TestVerify_Invalid(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signer := &rawSigner{}
	other := &rawSigner{}
	with := func(edit func(map[string]any)) map[string]any {
		c := validClaims(now)
		edit(c)
		return c
	}
	kid := signer.trusted(t).ID
	// rawHeader mints a token whose header is the JSON text header, signed
	// by signer: for headers a map can't spell, such as a member given twice.
	rawHeader := func(header string) string { return signer.signWithRawHeader(t, header, now) }
	// resigned replaces a valid token's signature with what edit makes of it.
	resigned := func(edit func(sig string) string) string {
		parts := strings.Split(signer.sign(t, validClaims(now)), ".")
		return parts[0] + "." + parts[1] + "." + edit(parts[2])
	}

	tests := map[string]struct {
		token      func() string
		wantReason string
	}{
		"not a JWS":     {token: func() string { return "a.b" }, wantReason: "JWS"},
		"four segments": {token: func() string { return signer.sign(t, validClaims(now)) + ".x" }, wantReason: "JWS"},
		"empty":         {token: func() string { return "" }, wantReason: "JWS"},
		// The signature: absent, cut short, re-encoded.
		"no signature":             {token: func() string { return resigned(func(string) string { return "" }) }, wantReason: "signature"},
		"a signature cut short":    {token: func() string { return resigned(func(sig string) string { return sig[:len(sig)-2] }) }, wantReason: "signature"},
		"a padded signature":       {token: func() string { return resigned(func(sig string) string { return sig + "==" }) }, wantReason: "signature"},
		"a signature that is text": {token: func() string { return resigned(func(string) string { return "not base64!" }) }, wantReason: "signature"},
		// The header names the key and nothing else: it can't bring one.
		"header is not base64url": {token: func() string { return "!!." + encodeRaw(t, "{}") + ".AA" }, wantReason: "header"},
		"header is not JSON":      {token: func() string { return rawHeader("EdDSA") }, wantReason: "header"},
		"header is not an object": {token: func() string { return rawHeader(`["EdDSA"]`) }, wantReason: "header"},
		"no alg":                  {token: func() string { return rawHeader(`{"kid":"` + kid + `"}`) }, wantReason: "EdDSA"},
		"alg in another case":     {token: func() string { return rawHeader(`{"alg":"eddsa","kid":"` + kid + `"}`) }, wantReason: "EdDSA"},
		"alg under another name":  {token: func() string { return rawHeader(`{"ALG":"EdDSA","kid":"` + kid + `"}`) }, wantReason: "EdDSA"},
		"alg given twice":         {token: func() string { return rawHeader(`{"alg":"none","alg":"EdDSA","kid":"` + kid + `"}`) }, wantReason: "header"},
		"kid given twice":         {token: func() string { return rawHeader(`{"alg":"EdDSA","kid":"other","kid":"` + kid + `"}`) }, wantReason: "header"},
		"no kid":                  {token: func() string { return rawHeader(`{"alg":"EdDSA"}`) }, wantReason: "not trusted"},
		"kid is not a string":     {token: func() string { return rawHeader(`{"alg":"EdDSA","kid":1}`) }, wantReason: "header"},
		"a key of its own in the header": {
			token: func() string {
				return other.signWithHeader(t, map[string]any{
					"alg": "EdDSA", "kid": kid,
					"jwk": map[string]any{"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(other.trusted(t).Public)},
				}, validClaims(now))
			},
			wantReason: "signature",
		},
		// RFC 7515 4.1.11: a header that must be understood, and isn't. Any
		// crit is refused, whatever it holds: none names a header this
		// verifier could have been told to understand.
		"a critical header":         {token: func() string { return rawHeader(`{"alg":"EdDSA","kid":"` + kid + `","crit":["exp"]}`) }, wantReason: "crit"},
		"crit names nothing":        {token: func() string { return rawHeader(`{"alg":"EdDSA","kid":"` + kid + `","crit":[]}`) }, wantReason: "crit"},
		"crit is null":              {token: func() string { return rawHeader(`{"alg":"EdDSA","kid":"` + kid + `","crit":null}`) }, wantReason: "crit"},
		"crit is not a list":        {token: func() string { return rawHeader(`{"alg":"EdDSA","kid":"` + kid + `","crit":"exp"}`) }, wantReason: "crit"},
		"crit names a known header": {token: func() string { return rawHeader(`{"alg":"EdDSA","kid":"` + kid + `","typ":"JWT","crit":["typ"]}`) }, wantReason: "crit"},
		// The claims: an object, with numbers where times go.
		"claims are not an object": {token: func() string { return signer.signWithRawClaims(t, encodeRaw(t, `[]`)) }, wantReason: "claims"},
		"claims are not JSON":      {token: func() string { return signer.signWithRawClaims(t, encodeRaw(t, `{`)) }, wantReason: "claims"},
		"exp is a string": {token: func() string {
			return signer.sign(t, with(func(c map[string]any) { c["exp"] = "1800000300" }))
		}, wantReason: "claims"},
		"alg is not EdDSA": {
			token: func() string {
				return signer.signWithHeader(t, map[string]any{"alg": "HS256", "kid": signer.trusted(t).ID}, validClaims(now))
			},
			wantReason: "EdDSA",
		},
		"alg none": {
			token: func() string {
				return signer.signWithHeader(t, map[string]any{"alg": "none", "kid": signer.trusted(t).ID}, validClaims(now))
			},
			wantReason: "EdDSA",
		},
		"unknown kid": {token: func() string { return other.sign(t, validClaims(now)) }, wantReason: "not trusted"},
		"signed by another key under a trusted kid": {
			token: func() string {
				return other.signWithHeader(t, map[string]any{"alg": "EdDSA", "kid": signer.trusted(t).ID}, validClaims(now))
			},
			wantReason: "signature",
		},
		"claims tampered after signing": {
			token: func() string {
				parts := strings.Split(signer.sign(t, validClaims(now)), ".")
				forged := strings.Split(signer.sign(t, with(func(c map[string]any) {
					c["path_auth"] = map[string]any{"root": "other", "pub": ""}
				})), ".")
				return parts[0] + "." + forged[1] + "." + parts[2]
			},
			wantReason: "signature",
		},
		"unknown claim":      {token: func() string { return signer.sign(t, with(func(c map[string]any) { c["role"] = "admin" })) }, wantReason: `"role"`},
		"sub with path_auth": {token: func() string { return signer.sign(t, with(func(c map[string]any) { c["sub"] = "user-1" })) }, wantReason: "sub goes with scopes"},
		"aud is refused":     {token: func() string { return signer.sign(t, with(func(c map[string]any) { c["aud"] = "qumo-relay" })) }, wantReason: `"aud"`},
		"no exp":             {token: func() string { return signer.sign(t, with(func(c map[string]any) { delete(c, "exp") })) }, wantReason: "required"},
		"no iat":             {token: func() string { return signer.sign(t, with(func(c map[string]any) { delete(c, "iat") })) }, wantReason: "required"},
		"no nbf":             {token: func() string { return signer.sign(t, with(func(c map[string]any) { delete(c, "nbf") })) }, wantReason: "required"},
		"no path_auth":       {token: func() string { return signer.sign(t, with(func(c map[string]any) { delete(c, "path_auth") })) }, wantReason: "path_auth"},
		"lifetime over 1 h": {token: func() string {
			return signer.sign(t, with(func(c map[string]any) { c["exp"] = now.Add(61 * time.Minute).Unix() }))
		}, wantReason: "lifetime"},
		"exp before iat": {token: func() string {
			return signer.sign(t, with(func(c map[string]any) { c["exp"] = now.Add(-time.Minute).Unix() }))
		}, wantReason: "lifetime"},
		"expired beyond the leeway": {
			token: func() string {
				return signer.sign(t, with(func(c map[string]any) {
					c["iat"], c["nbf"], c["exp"] = now.Add(-20*time.Minute).Unix(), now.Add(-20*time.Minute).Unix(), now.Add(-61*time.Second).Unix()
				}))
			},
			wantReason: "expired",
		},
		"nbf beyond the leeway": {
			token: func() string {
				return signer.sign(t, with(func(c map[string]any) { c["nbf"] = now.Add(2 * time.Minute).Unix() }))
			},
			wantReason: "not valid yet",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := Verify(tt.token(), signer.keys(t), now)

			require.ErrorIs(t, err, ErrInvalid)
			assert.Contains(t, err.Error(), tt.wantReason)
			assert.Equal(t, Claims{}, got, "a refused token grants nothing")
		})
	}
}

func TestVerify_DuplicateClaimIsInvalid(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signer := &rawSigner{}
	// A second exp, after the one a lenient decoder would read, must not pass.
	forged := signer.signWithRawClaims(t, encodeRaw(t,
		`{"path_auth":{"root":"acme","pub":""},"iat":1800000000,"nbf":1800000000,"exp":1800000600,"exp":1900000000}`))

	_, err := Verify(forged, signer.keys(t), now)

	require.ErrorIs(t, err, ErrInvalid)
}

func TestVerify_LeewayAdmitsSmallSkew(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signer := &rawSigner{}
	c := validClaims(now)
	// Not valid for another 30 s, and expired 30 s ago: both within 60 s.
	c["iat"] = now.Add(-10 * time.Minute).Unix()
	c["nbf"] = now.Add(30 * time.Second).Unix()
	c["exp"] = now.Add(-30 * time.Second).Unix()

	_, err := Verify(signer.sign(t, c), signer.keys(t), now)

	assert.NoError(t, err)
}

// A path_auth claim, as other MoQ implementations sign it, is read as the
// scopes it amounts to.
func TestVerify_PathConfinement(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tests := map[string]struct {
		prefix   string
		pathAuth map[string]any
		want     Grant
		wantErr  error
	}{
		"publish and subscribe": {
			pathAuth: map[string]any{"root": "acme/app", "pub": "alice", "sub": ""},
			want:     Grant{Scopes: []Scope{pubScope("acme/app/alice"), subScope("acme/app")}},
		},
		"subscribe only": {
			pathAuth: map[string]any{"root": "acme/app", "sub": "live"},
			want:     Grant{Scopes: []Scope{subScope("acme/app/live")}},
		},
		"slashes normalized": {
			pathAuth: map[string]any{"root": "/acme//app/", "pub": "/alice/"},
			want:     pubGrant("acme/app/alice"),
		},
		"within the key's prefix": {
			prefix:   "acme/app",
			pathAuth: map[string]any{"root": "acme/app", "pub": "alice"},
			want:     pubGrant("acme/app/alice"),
		},
		"exactly the key's prefix": {
			prefix:   "acme/app",
			pathAuth: map[string]any{"root": "acme/app", "pub": ""},
			want:     pubGrant("acme/app"),
		},
		"another tenant's path":             {prefix: "acme/app", pathAuth: map[string]any{"root": "other/app", "pub": ""}, wantErr: ErrForbidden},
		"a sibling sharing a string prefix": {prefix: "acme/app", pathAuth: map[string]any{"root": "acme/apple", "pub": ""}, wantErr: ErrForbidden},
		"a parent of the key's prefix":      {prefix: "acme/app", pathAuth: map[string]any{"root": "acme", "sub": ""}, wantErr: ErrForbidden},
		"only sub escapes the prefix":       {prefix: "acme/app", pathAuth: map[string]any{"root": "acme/app", "pub": "alice", "sub": "../../other"}, wantErr: ErrInvalid},
		"dot-dot in root":                   {pathAuth: map[string]any{"root": "acme/app/../../other", "pub": ""}, wantErr: ErrInvalid},
		"a wildcard segment":                {pathAuth: map[string]any{"root": "acme/*", "pub": ""}, wantErr: ErrInvalid},
		"names no path":                     {pathAuth: map[string]any{"root": "", "pub": ""}, wantErr: ErrInvalid},
		"grants nothing":                    {pathAuth: map[string]any{"root": "acme/app"}, wantErr: ErrForbidden},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			signer := &rawSigner{prefix: tt.prefix}
			c := validClaims(now)
			c["path_auth"] = tt.pathAuth

			got, err := Verify(signer.sign(t, c), signer.keys(t), now)

			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.Grant)
		})
	}
}

func TestSign_RoundTrip(t *testing.T) {
	key, err := GenerateKey("acme/app")
	require.NoError(t, err)
	now := time.Unix(1_800_000_000, 0)

	tok, err := signAt(key, Grant{Scopes: []Scope{pubScope("/acme/app/rooms/42/alice/"), subScope("acme/app/rooms/42")}}, 30*time.Minute, now)
	require.NoError(t, err)
	c, err := Verify(tok, map[string]Key{key.ID: key.Public()}, now)

	require.NoError(t, err)
	assert.Equal(t, Grant{Scopes: []Scope{pubScope("acme/app/rooms/42/alice"), subScope("acme/app/rooms/42")}}, c.Grant)
	assert.Equal(t, now.Add(30*time.Minute), c.ExpiresAt)
	assert.Len(t, c.ID, 26, "a random jti (rand.Text)")
}

func TestSign_Refusals(t *testing.T) {
	tests := map[string]struct {
		prefix      string
		grant       Grant
		ttl         time.Duration
		wantErrText string
	}{
		"zero ttl":        {grant: pubGrant("a"), ttl: 0, wantErrText: "ttl"},
		"ttl over 1 h":    {grant: pubGrant("a"), ttl: 61 * time.Minute, wantErrText: "ttl"},
		"grants nothing":  {grant: Grant{}, ttl: time.Minute, wantErrText: "no scope"},
		"dot-dot path":    {grant: pubGrant("a/../b"), ttl: time.Minute, wantErrText: "segments"},
		"wildcard path":   {grant: Grant{Scopes: []Scope{subScope("a/*")}}, ttl: time.Minute, wantErrText: "segments"},
		"path of slashes": {grant: pubGrant("//"), ttl: time.Minute, wantErrText: "segments"},
		// A verifier would refuse it, so it is never signed.
		"publish outside the key's prefix":   {prefix: "acme/app", grant: pubGrant("other/app"), ttl: time.Minute, wantErrText: "outside"},
		"subscribe outside the key's prefix": {prefix: "acme/app", grant: Grant{Scopes: []Scope{subScope("acme/application")}}, ttl: time.Minute, wantErrText: "outside"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			key, err := GenerateKey(tt.prefix)
			require.NoError(t, err)

			_, err = Sign(key, tt.grant, tt.ttl)

			assert.ErrorContains(t, err, tt.wantErrText)
		})
	}
}

// pubScope is the scope a path_auth pub of path amounts to.
func pubScope(path string) Scope {
	return Scope{Actions: []Action{ActionPublish}, Broadcast: path, Prefix: true}
}

// subScope is the scope a path_auth sub of path amounts to.
func subScope(path string) Scope {
	return Scope{Actions: []Action{ActionSubscribe, ActionFetch}, Broadcast: path, Prefix: true}
}

func pubGrant(path string) Grant {
	return Grant{Scopes: []Scope{pubScope(path)}}
}
