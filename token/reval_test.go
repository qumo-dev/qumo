package token

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerify_Reval(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tests := map[string]struct {
		reval     any // nil: no reval claim
		want      time.Duration
		wantError bool
	}{
		"absent":               {want: 0},
		"the floor":            {reval: 30, want: MinReval},
		"a minute":             {reval: 60, want: time.Minute},
		"a fraction":           {reval: 45.5, want: 45500 * time.Millisecond},
		"the lifetime cap":     {reval: 3600, want: MaxLifetime},
		"below the floor":      {reval: 29, wantError: true},
		"zero":                 {reval: 0, wantError: true},
		"negative":             {reval: -60, wantError: true},
		"above the lifetime":   {reval: 3601, wantError: true},
		"too large to be time": {reval: 1e300, wantError: true},
		"a string":             {reval: "60", wantError: true},
		"a list":               {reval: []int{60}, wantError: true},
		"an object":            {reval: map[string]any{"s": 60}, wantError: true},
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
		"with reval, valid":                 {token: with(60), keys: trusted, at: issued},
		"with reval, past its exp":          {token: with(60), keys: trusted, at: expired, wantErr: ErrInvalid},
		"without reval, its key withdrawn":  {token: plain, keys: withdrawn, at: expired, wantErr: ErrInvalid},
		"without reval, outside the prefix": {token: plain, keys: confined, at: expired, wantErr: ErrForbidden},
		"reval below the floor":             {token: with(1), keys: trusted, at: issued, wantErr: ErrInvalid},
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
		opts        []Option
		want        time.Duration
		wantErrText string
	}{
		"none":                   {want: 0},
		"two minutes":            {opts: []Option{WithReval(2 * time.Minute)}, want: 2 * time.Minute},
		"whole seconds":          {opts: []Option{WithReval(90*time.Second + 400*time.Millisecond)}, want: 90 * time.Second},
		"below the floor":        {opts: []Option{WithReval(MinReval - time.Second)}, wantErrText: "reval"},
		"above the lifetime cap": {opts: []Option{WithReval(MaxLifetime + time.Second)}, wantErrText: "reval"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			tok, err := signAt(key, Grant{Scopes: []Scope{subScope("acme/app")}}, 10*time.Minute, now, tt.opts...)
			if tt.wantErrText != "" {
				assert.ErrorContains(t, err, tt.wantErrText)
				return
			}
			require.NoError(t, err)

			c, err := Verify(tok, keys, now)

			require.NoError(t, err)
			assert.Equal(t, tt.want, c.Reval)
		})
	}
}
