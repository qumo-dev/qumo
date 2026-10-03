package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"

	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestServer_HandleWebTransport_Refused(t *testing.T) {
	tests := map[string]struct {
		err        error
		wantStatus int
	}{
		"401 from the auth server":   {err: auth.RefusedError{Status: http.StatusUnauthorized}, wantStatus: http.StatusUnauthorized},
		"403 from the auth server":   {err: auth.RefusedError{Status: http.StatusForbidden}, wantStatus: http.StatusForbidden},
		"auth server unavailable":    {err: errors.New("connection refused"), wantStatus: http.StatusServiceUnavailable},
		"grant the relay can't keep": {err: fmt.Errorf("%w: mounts", auth.ErrInvalidGrant), wantStatus: http.StatusServiceUnavailable},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fake := &fakeAuth{err: tt.err}
			srv := newTestServer("127.0.0.1:0")
			srv.authorize = fake.authorize
			t.Cleanup(func() { _ = srv.Close() })
			// A WebTransport upgrade is an extended CONNECT whose :path is the
			// URL's path; httptest parses a CONNECT target as an authority, so
			// build the URL first and set the method after.
			req := httptest.NewRequest(http.MethodGet, "https://relay.example/acme/app?jwt=a.b.c", nil)
			req.Method = http.MethodConnect
			req.RemoteAddr = "192.0.2.1:5000"
			rec := httptest.NewRecorder()

			srv.HandleWebTransport(rec, req)

			assert.Equal(t, tt.wantStatus, rec.Code)
			got := fake.received()
			require.Len(t, got, 1, "the auth server is asked once per upgrade")
			assert.Equal(t, auth.EventConnect, got[0].Event)
			assert.Equal(t, auth.TransportWebTransport, got[0].Transport)
			assert.Equal(t, "/acme/app", got[0].Path)
			assert.Equal(t, "jwt=a.b.c", got[0].Query, "the credential is forwarded unparsed")
			assert.Equal(t, "192.0.2.1:5000", got[0].Remote)
			assert.Len(t, got[0].ID, 32)
		})
	}
}

func TestServer_Admit_Metrics(t *testing.T) {
	tests := map[string]struct {
		err        error
		wantResult string
	}{
		"admitted":    {wantResult: "admitted"},
		"refused":     {err: auth.RefusedError{Status: http.StatusForbidden}, wantResult: "refused"},
		"invalid":     {err: fmt.Errorf("%w: root", auth.ErrInvalidGrant), wantResult: "invalid"},
		"unavailable": {err: errors.New("timeout"), wantResult: "error"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fake := &fakeAuth{err: tt.err}
			srv := &Server{authorize: fake.authorize}
			counter := metricAuthRequests.WithLabelValues(auth.EventConnect, tt.wantResult)
			var err error

			delta := counterDelta(t, func() {
				_, err = srv.admit(context.Background(), auth.Request{Event: auth.EventConnect})
			}, counter)

			assert.Equal(t, 1.0, delta)
			assert.ErrorIs(t, err, tt.err)
		})
	}
}
