package relay

import (
	"context"
	"net/http"
	"testing"

	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewAdmitter(t *testing.T) {
	tests := map[string]struct {
		cfg         auth.Config
		want        admitter
		wantErrText string
	}{
		"auth server":  {cfg: auth.Config{URL: "https://auth.example.com/"}, want: &auth.Client{}},
		"public grant": {cfg: auth.Config{}, want: publicGrant{}},
		"bad URL":      {cfg: auth.Config{URL: "http://auth.example.com/"}, wantErrText: "QUMO_AUTH_URL"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := newAdmitter(tt.cfg)

			if tt.wantErrText != "" {
				assert.ErrorContains(t, err, tt.wantErrText)
				return
			}
			require.NoError(t, err)
			assert.IsType(t, tt.want, got)
		})
	}
}

func TestPublicGrant_Connect(t *testing.T) {
	g := &auth.Grant{}
	p := publicGrant{grant: g}
	tests := map[string]struct {
		query      string
		wantStatus int // 0 means admitted on the public grant
	}{
		"no credential":                  {query: ""},
		"other query":                    {query: "room=1"},
		"a credential is refused":        {query: "jwt=a.b.c", wantStatus: http.StatusUnauthorized},
		"an empty credential is refused": {query: "jwt=", wantStatus: http.StatusUnauthorized},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := p.Connect(context.Background(), auth.Request{Path: "/", Query: tt.query})

			if tt.wantStatus != 0 {
				require.Error(t, err)
				assert.Equal(t, tt.wantStatus, auth.RefusalStatus(err))
				return
			}
			require.NoError(t, err)
			assert.Same(t, g, got)
		})
	}
}
