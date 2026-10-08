package token

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerify_Reval(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tests := map[string]struct {
		reval     any // nil: no reval claim
		want      bool
		wantError bool
	}{
		"absent":                 {want: false},
		"the relay's interval":   {reval: 30, want: true},
		"a minute":               {reval: 60, want: true},
		"a fraction above":       {reval: 30.5, want: true},
		"beyond the lifetime":    {reval: 7200, want: true},
		"huge":                   {reval: 1e300, want: true},
		"just below the relay's": {reval: 29.9, wantError: true},
		"below the relay's":      {reval: 1, wantError: true},
		"zero":                   {reval: 0, wantError: true},
		"negative":               {reval: -60, wantError: true},
		"a string":               {reval: "60", wantError: true},
		"true":                   {reval: true, wantError: true},
		"a list":                 {reval: []int{60}, wantError: true},
		"an object":              {reval: map[string]any{"s": 60}, wantError: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			signer := &rawSigner{}
			c := validClaims(now)
			if tt.reval != nil {
				c["reval"] = tt.reval
			}

			got, err := Verify(signer.sign(t, c), signer.keys(t), now)

			if tt.wantError {
				assert.ErrorIs(t, err, ErrInvalid)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.Reval)
		})
	}
}

// A reval of null is not a number, and refuses the token rather than reading
// as absent.
func TestVerify_RevalNull(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signer := &rawSigner{}
	c := validClaims(now)
	c["reval"] = nil

	_, err := Verify(signer.sign(t, c), signer.keys(t), now)

	assert.ErrorIs(t, err, ErrInvalid)
}

func TestVerifyLive(t *testing.T) {
	issued := time.Unix(1_800_000_000, 0)
	expired := issued.Add(10*time.Minute + Leeway)
	signer := &rawSigner{}
	with := func(reval any) string {
		c := validClaims(issued)
		c["reval"] = reval
		return signer.sign(t, c)
	}
	plain := signer.sign(t, validClaims(issued))
	trusted := signer.keys(t)
	withdrawn := (&rawSigner{}).keys(t)
	confined := (&rawSigner{prefix: "other", private: signer.private}).keys(t)

	tests := map[string]struct {
		token   string
		keys    map[string]Key
		at      time.Time
		wantErr error
	}{
		"without reval, valid":              {token: plain, keys: trusted, at: issued},
		"without reval, past its exp":       {token: plain, keys: trusted, at: expired},
		"with reval, valid":                 {token: with(30), keys: trusted, at: issued},
		"with reval, past its exp":          {token: with(30), keys: trusted, at: expired, wantErr: ErrInvalid},
		"without reval, its key withdrawn":  {token: plain, keys: withdrawn, at: expired, wantErr: ErrInvalid},
		"without reval, outside the prefix": {token: plain, keys: confined, at: expired, wantErr: ErrForbidden},
		"reval below the relay's interval":  {token: with(1), keys: trusted, at: issued, wantErr: ErrInvalid},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := VerifyLive(tt.token, tt.keys, tt.at)

			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				assert.Equal(t, Claims{}, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, Grant{Scopes: []Scope{pubScope("acme/app/alice"), subScope("acme/app")}}, got.Grant)
		})
	}
}

func TestSign_Reval(t *testing.T) {
	key, err := GenerateKey("acme/app")
	require.NoError(t, err)
	now := time.Unix(1_800_000_000, 0)
	keys := map[string]Key{key.ID: key.Public()}

	tests := map[string]struct {
		reval     bool
		wantClaim string // the reval claim as encoded; empty: absent
	}{
		"off": {reval: false},
		"on":  {reval: true, wantClaim: `"reval":30`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			tok, err := signAt(key, Grant{Scopes: []Scope{subScope("acme/app")}}, Options{TTL: 10 * time.Minute, Reval: tt.reval}, now)
			require.NoError(t, err)

			c, err := Verify(tok, keys, now)

			require.NoError(t, err)
			assert.Equal(t, tt.reval, c.Reval)
			body, err := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[1])
			require.NoError(t, err)
			if tt.wantClaim == "" {
				assert.NotContains(t, string(body), `"reval"`)
				return
			}
			assert.Contains(t, string(body), tt.wantClaim)
		})
	}
}
