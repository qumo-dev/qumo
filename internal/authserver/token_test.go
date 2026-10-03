package authserver

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerify_AcceptsAValidToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signer := &fakeSigner{prefix: ""}

	c, key, err := verify(signer.sign(t, validClaims(now)), signer.keys(t), now)

	require.NoError(t, err)
	assert.Equal(t, signer.trusted(t).ID, key.ID)
	assert.Equal(t, "acme/app", c.PathAuth.Root)
	assert.Equal(t, "j-1", c.ID)
}

func TestVerify_Refusals(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signer := &fakeSigner{prefix: ""}
	other := &fakeSigner{prefix: ""}
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
			_, _, err := verify(tt.token(), signer.keys(t), now)

			var refused refusal
			require.ErrorAs(t, err, &refused)
			assert.Equal(t, http.StatusUnauthorized, refused.status)
			assert.Contains(t, refused.reason, tt.wantReason)
		})
	}
}

func TestVerify_LeewayAdmitsSmallSkew(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signer := &fakeSigner{prefix: ""}
	c := validClaims(now)
	// Not valid for another 30 s, and expired 30 s ago: both within 60 s.
	c["iat"] = now.Add(-10 * time.Minute).Unix()
	c["nbf"] = now.Add(30 * time.Second).Unix()
	c["exp"] = now.Add(-30 * time.Second).Unix()

	_, _, err := verify(signer.sign(t, c), signer.keys(t), now)

	assert.NoError(t, err)
}
