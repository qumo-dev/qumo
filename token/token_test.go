package token

import (
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
	assert.Equal(t, Grant{Publish: "acme/app/alice", Subscribe: "acme/app"}, c.Grant)
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

	tests := map[string]struct {
		token      func() string
		wantReason string
	}{
		"not a JWS": {token: func() string { return "a.b" }, wantReason: "JWS"},
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
		"unknown claim":  {token: func() string { return signer.sign(t, with(func(c map[string]any) { c["role"] = "admin" })) }, wantReason: `"role"`},
		"sub is refused": {token: func() string { return signer.sign(t, with(func(c map[string]any) { c["sub"] = "user-1" })) }, wantReason: `"sub"`},
		"aud is refused": {token: func() string { return signer.sign(t, with(func(c map[string]any) { c["aud"] = "qumo-relay" })) }, wantReason: `"aud"`},
		"no exp":         {token: func() string { return signer.sign(t, with(func(c map[string]any) { delete(c, "exp") })) }, wantReason: "required"},
		"no iat":         {token: func() string { return signer.sign(t, with(func(c map[string]any) { delete(c, "iat") })) }, wantReason: "required"},
		"no nbf":         {token: func() string { return signer.sign(t, with(func(c map[string]any) { delete(c, "nbf") })) }, wantReason: "required"},
		"no path_auth":   {token: func() string { return signer.sign(t, with(func(c map[string]any) { delete(c, "path_auth") })) }, wantReason: "path_auth"},
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
			_, err := Verify(tt.token(), signer.keys(t), now)

			require.ErrorIs(t, err, ErrInvalid)
			assert.Contains(t, err.Error(), tt.wantReason)
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
			want:     Grant{Publish: "acme/app/alice", Subscribe: "acme/app"},
		},
		"subscribe only": {
			pathAuth: map[string]any{"root": "acme/app", "sub": "live"},
			want:     Grant{Subscribe: "acme/app/live"},
		},
		"slashes normalized": {
			pathAuth: map[string]any{"root": "/acme//app/", "pub": "/alice/"},
			want:     Grant{Publish: "acme/app/alice"},
		},
		"within the key's prefix": {
			prefix:   "acme/app",
			pathAuth: map[string]any{"root": "acme/app", "pub": "alice"},
			want:     Grant{Publish: "acme/app/alice"},
		},
		"exactly the key's prefix": {
			prefix:   "acme/app",
			pathAuth: map[string]any{"root": "acme/app", "pub": ""},
			want:     Grant{Publish: "acme/app"},
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

	tok, err := signAt(key, Grant{Publish: "/acme/app/rooms/42/alice/", Subscribe: "acme/app/rooms/42"}, 30*time.Minute, now)
	require.NoError(t, err)
	c, err := Verify(tok, map[string]Key{key.ID: key.Public()}, now)

	require.NoError(t, err)
	assert.Equal(t, Grant{Publish: "acme/app/rooms/42/alice", Subscribe: "acme/app/rooms/42"}, c.Grant)
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
		"zero ttl":        {grant: Grant{Publish: "a"}, ttl: 0, wantErrText: "ttl"},
		"ttl over 1 h":    {grant: Grant{Publish: "a"}, ttl: 61 * time.Minute, wantErrText: "ttl"},
		"grants nothing":  {grant: Grant{}, ttl: time.Minute, wantErrText: "neither"},
		"dot-dot path":    {grant: Grant{Publish: "a/../b"}, ttl: time.Minute, wantErrText: "segments"},
		"wildcard path":   {grant: Grant{Subscribe: "a/*"}, ttl: time.Minute, wantErrText: "segments"},
		"path of slashes": {grant: Grant{Publish: "//"}, ttl: time.Minute, wantErrText: "segments"},
		// A verifier would refuse it, so it is never signed.
		"publish outside the key's prefix":   {prefix: "acme/app", grant: Grant{Publish: "other/app"}, ttl: time.Minute, wantErrText: "outside"},
		"subscribe outside the key's prefix": {prefix: "acme/app", grant: Grant{Subscribe: "acme/application"}, ttl: time.Minute, wantErrText: "outside"},
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
