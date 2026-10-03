package relay

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewAdmitter(t *testing.T) {
	tests := map[string]struct {
		cfg         auth.Config
		want        string
		wantErrText string
	}{
		"auth server":  {cfg: auth.Config{URL: "https://auth.example.com/"}, want: "https://auth.example.com/"},
		"public grant": {cfg: auth.Config{}, want: "public grant"},
		"bad URL":      {cfg: auth.Config{URL: "http://auth.example.com/"}, wantErrText: "QUMO_AUTH_URL"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			a, err := newAdmitter(tt.cfg)

			if tt.wantErrText != "" {
				assert.ErrorContains(t, err, tt.wantErrText)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, a.String())
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

func TestSessionGrant(t *testing.T) {
	g := &auth.Grant{}
	tests := map[string]struct {
		ctx  context.Context
		want *auth.Grant
	}{
		"no admission is unchecked": {ctx: context.Background(), want: nil},
		"decided with a grant":      {ctx: withAdmission(context.Background(), decidedAdmission(g)), want: g},
		"decided unchecked":         {ctx: withAdmission(context.Background(), decidedAdmission(nil)), want: nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := sessionGrant(tt.ctx)

			require.NoError(t, err)
			assert.Same(t, tt.want, got)
		})
	}
}

func TestSessionGrant_WaitsForPendingAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := pendingAdmission()
		ctx := withAdmission(context.Background(), a)
		g := &auth.Grant{}
		var got *auth.Grant
		var err error
		done := make(chan struct{})
		go func() {
			defer close(done)
			got, err = sessionGrant(ctx)
		}()

		synctest.Wait() // sessionGrant is blocked on the pending admission
		select {
		case <-done:
			t.Fatal("sessionGrant returned before the admission was decided")
		default:
		}
		a.decide(g)
		<-done

		require.NoError(t, err)
		assert.Same(t, g, got)
	})
}

func TestSessionGrant_ContextEndsWhilePending(t *testing.T) {
	ctx, cancel := context.WithCancel(withAdmission(context.Background(), pendingAdmission()))
	cancel()

	got, err := sessionGrant(ctx)

	assert.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, got)
}
