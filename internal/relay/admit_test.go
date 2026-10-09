package relay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"

	"github.com/qumo-dev/gomoqt/moqt"
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
		"decided with a grant":      {ctx: withAdmission(context.Background(), decidedAdmission(g, auth.Request{})), want: g},
		"decided unchecked":         {ctx: withAdmission(context.Background(), decidedAdmission(nil, auth.Request{})), want: nil},
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
		a.decide(g, auth.Request{})
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
		"refused with 401":     {err: auth.RefusedError{Status: http.StatusUnauthorized}, wantStatus: http.StatusUnauthorized},
		"refused with 403":     {err: auth.RefusedError{Status: http.StatusForbidden}, wantStatus: http.StatusForbidden},
		"can't be checked now": {err: errors.New("no key set loaded yet"), wantStatus: http.StatusServiceUnavailable},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fake := &fakeAuth{err: tt.err}
			srv := newTestServer("127.0.0.1:0")
			srv.Authorize = fake.authorize
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
			require.Len(t, got, 1, "an upgrade is checked once")
			assert.Equal(t, auth.EventConnect, got[0].Event)
			assert.Equal(t, auth.TransportWebTransport, got[0].Transport)
			assert.Equal(t, "/acme/app", got[0].Path)
			assert.Equal(t, "jwt=a.b.c", got[0].Query, "the credential is forwarded unparsed")
			assert.Equal(t, "192.0.2.1:5000", got[0].Remote)
			assert.Len(t, got[0].ID, 32)
		})
	}
}

func TestServer_HandleWebTransport_NotAnUpgrade(t *testing.T) {
	fake := &fakeAuth{}
	srv := newTestServer("127.0.0.1:0")
	srv.Authorize = fake.authorize
	t.Cleanup(func() { _ = srv.Close() })
	rec := httptest.NewRecorder()

	srv.HandleWebTransport(rec, httptest.NewRequest(http.MethodGet, "https://relay.example/?jwt=a.b.c", nil))

	assert.Empty(t, fake.received(), "a request that isn't an upgrade is never checked")
	assert.Equal(t, http.StatusBadRequest, rec.Code, "gomoqt answers it without a session")
}

func TestServer_HandleWebTransport_NoAuthorizeRefuses(t *testing.T) {
	srv := newTestServer("127.0.0.1:0") // authorize left unset
	t.Cleanup(func() { _ = srv.Close() })
	req := httptest.NewRequest(http.MethodGet, "https://relay.example/acme/app", nil)
	req.Method = http.MethodConnect
	rec := httptest.NewRecorder()
	counter := metricAuthRequests.WithLabelValues(auth.EventConnect, "error")

	delta := counterDelta(t, func() { srv.HandleWebTransport(rec, req) }, counter)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "a Server without authorize never runs open")
	assert.Equal(t, 1.0, delta)
}

func TestServer_Admit_Metrics(t *testing.T) {
	tests := map[string]struct {
		authorize  func(context.Context, auth.Request) (*auth.Grant, error)
		err        error
		wantResult string
	}{
		"auth off":    {authorize: admitUnchecked, wantResult: "unchecked"},
		"admitted":    {wantResult: "admitted"},
		"refused":     {err: auth.RefusedError{Status: http.StatusForbidden}, wantResult: "refused"},
		"unavailable": {err: errors.New("timeout"), wantResult: "error"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			authorize := tt.authorize
			if authorize == nil {
				authorize = (&fakeAuth{err: tt.err}).authorize
			}
			srv := &Server{Authorize: authorize}
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

func TestServer_ReportEnd(t *testing.T) {
	stats := moqt.SessionStats{BytesSent: 9000, BytesReceived: 120}
	tests := map[string]struct {
		endErr     error
		wantResult string
	}{
		"reported":       {wantResult: authOK},
		"server failing": {endErr: errors.New("503"), wantResult: authError},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			server := &fakeAuth{endErr: tt.endErr}
			srv := &Server{End: server.end}
			connect := auth.Request{ID: "00ff", Event: auth.EventConnect, Path: "/acme", Query: "jwt=h.p.s"}

			delta := counterDelta(t, func() {
				srv.reportEnd(context.Background(), connect, stats, endDropped)
			}, metricAuthRequests.WithLabelValues(auth.EventEnd, tt.wantResult))

			assert.Equal(t, 1.0, delta)
			want := connect
			want.Event = auth.EventEnd
			want.Bytes = auth.Bytes{Sent: 9000, Received: 120}
			want.Reason = endDropped
			assert.Equal(t, []auth.Request{want}, server.ended())
		})
	}
}

// TestServer_ReportEnd_Off verifies a Server without End, as with auth off,
// reports nothing.
func TestServer_ReportEnd_Off(t *testing.T) {
	delta := counterDelta(t, func() {
		(&Server{}).reportEnd(context.Background(), auth.Request{ID: "00ff"}, moqt.SessionStats{}, endClosed)
	}, metricAuthRequests.WithLabelValues(auth.EventEnd, authOK))

	assert.Zero(t, delta)
}

// TestServer_HandleWebTransport_DisallowedOriginNotAsked verifies a request
// from an Origin the upgrade refuses is never checked, which
// would otherwise count a session that never starts.
func TestServer_HandleWebTransport_DisallowedOriginNotAsked(t *testing.T) {
	fake := &fakeAuth{grant: testGrant(t, "", "**", 0)}
	srv := newTestServer("127.0.0.1:0")
	srv.Authorize = fake.authorize
	srv.End = fake.end
	srv.AllowedOrigins = []string{"https://good.example"}
	t.Cleanup(func() { _ = srv.Close() })
	req := httptest.NewRequest(http.MethodGet, "https://relay.example/acme?jwt=a.b.c", nil)
	req.Method = http.MethodConnect
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()

	srv.HandleWebTransport(rec, req)

	assert.Empty(t, fake.received(), "a refused Origin is never checked")
	assert.Empty(t, fake.ended())
	assert.Equal(t, http.StatusBadRequest, rec.Code, "gomoqt refuses the upgrade as before")
}

// TestServer_HandleWebTransport_FailedUpgradeReportsEnd verifies an admitted
// upgrade that fails anyway reports an end, which closes the
// session it counted at connect.
func TestServer_HandleWebTransport_FailedUpgradeReportsEnd(t *testing.T) {
	fake := &fakeAuth{grant: testGrant(t, "", "**", 0)}
	srv := newTestServer("127.0.0.1:0")
	srv.Authorize = fake.authorize
	srv.End = fake.end
	t.Cleanup(func() { _ = srv.Close() })
	// An extended CONNECT without the WebTransport headers: admitted, then
	// refused by the upgrade.
	req := httptest.NewRequest(http.MethodGet, "https://relay.example/acme?jwt=a.b.c", nil)
	req.Method = http.MethodConnect
	rec := httptest.NewRecorder()

	srv.HandleWebTransport(rec, req)

	require.Len(t, fake.received(), 1)
	connect := fake.received()[0]
	want := connect
	want.Event = auth.EventEnd
	want.Reason = endUpgradeFailed
	assert.Equal(t, []auth.Request{want}, fake.ended())
}
