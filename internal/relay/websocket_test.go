package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newWebSocketTestServer returns a Server that takes WebSocket upgrades,
// asking fake about each session.
func newWebSocketTestServer(tb testing.TB, fake *fakeAuth) *Server {
	tb.Helper()
	srv := newTestServer("127.0.0.1:0")
	srv.Config = &Config{NodeID: "relay-ws-test", WebSocket: true}
	srv.Authorize = fake.authorize
	srv.End = fake.end
	tb.Cleanup(func() { _ = srv.Close() })
	return srv
}

// webSocketUpgrade returns a WebSocket upgrade request for target that
// offers MoQ over QMux. It carries no Sec-WebSocket-Key, so the upgrade
// itself fails: it is for what happens before that.
func webSocketUpgrade(target string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Protocol", moqt.NextProtoQMux)
	req.RemoteAddr = "192.0.2.1:5000"
	return req
}

// dialWebSocket serves srv's client endpoint on a test server and dials it
// over WebSocket.
func dialWebSocket(tb testing.TB, srv *Server, pathAndQuery string) (*moqt.Session, error) {
	tb.Helper()
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.HandleWebTransport))
	tb.Cleanup(httpSrv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	tb.Cleanup(cancel)
	url := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + pathAndQuery
	return (&moqt.Dialer{}).Dial(ctx, url, moqt.NewTrackMux(0))
}

// A browser cannot read the status of a refused WebSocket handshake, so a
// refused session is upgraded and then closed with the reason.
func TestServer_HandleWebTransport_WebSocketRefused(t *testing.T) {
	tests := map[string]struct {
		err        error
		wantReason string
	}{
		"refused with 401":     {err: auth.RefusedError{Status: http.StatusUnauthorized}, wantReason: refusalRefused},
		"refused with 403":     {err: auth.RefusedError{Status: http.StatusForbidden}, wantReason: refusalRefused},
		"can't be checked now": {err: errors.New("no key set loaded yet"), wantReason: refusalUnavailable},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fake := &fakeAuth{err: tt.err}
			srv := newWebSocketTestServer(t, fake)
			served := metricSessionsTotal.WithLabelValues(auth.TransportWebSocket)
			servedBefore := testutil.ToFloat64(served)

			sess, err := dialWebSocket(t, srv, "/acme/app?jwt=a.b.c")
			require.NoError(t, err, "the upgrade goes through, so that the client can be told why")

			select {
			case <-sess.Context().Done():
			case <-time.After(5 * time.Second):
				require.FailNow(t, "a refused session stayed open")
			}
			var serr *moqt.SessionError
			require.ErrorAs(t, moqt.Cause(sess.Context()), &serr)
			assert.True(t, serr.Remote)
			assert.Equal(t, moqt.UnauthorizedSessionErrorCode, serr.SessionErrorCode())
			assert.Equal(t, tt.wantReason, serr.ErrorMessage)

			got := fake.received()
			require.Len(t, got, 1, "an upgrade is checked once")
			assert.Equal(t, auth.EventConnect, got[0].Event)
			assert.Equal(t, auth.TransportWebSocket, got[0].Transport)
			assert.Equal(t, "/acme/app", got[0].Path)
			assert.Equal(t, "jwt=a.b.c", got[0].Query, "the credential is forwarded unparsed")
			assert.Len(t, got[0].ID, 32)
			assert.Empty(t, fake.ended(), "a refused session reports no end")
			assert.Equal(t, servedBefore, testutil.ToFloat64(served), "a refused session is not served")
		})
	}
}

// A request the upgrade would refuse anyway is never checked, which would
// otherwise count a session that never starts.
func TestServer_HandleWebTransport_WebSocketNotAsked(t *testing.T) {
	tests := map[string]struct {
		webSocket  bool
		origins    []string
		prepare    func(r *http.Request)
		wantStatus int
	}{
		"WebSocket is off": {
			webSocket:  false,
			prepare:    func(*http.Request) {},
			wantStatus: http.StatusBadRequest,
		},
		"an Origin that is not allowed": {
			webSocket:  true,
			origins:    []string{"https://good.example"},
			prepare:    func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
			wantStatus: http.StatusForbidden,
		},
		"no subprotocol": {
			webSocket:  true,
			prepare:    func(r *http.Request) { r.Header.Del("Sec-WebSocket-Protocol") },
			wantStatus: http.StatusBadRequest,
		},
		"another QMux draft": {
			webSocket:  true,
			prepare:    func(r *http.Request) { r.Header.Set("Sec-WebSocket-Protocol", "qmux-01.moq-lite-05, webtransport") },
			wantStatus: http.StatusBadRequest,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fake := &fakeAuth{grant: testGrant(t, "", "**", 0)}
			srv := newWebSocketTestServer(t, fake)
			srv.Config.WebSocket = tt.webSocket
			srv.AllowedOrigins = tt.origins
			req := webSocketUpgrade("https://relay.example/acme?jwt=a.b.c")
			tt.prepare(req)
			rec := httptest.NewRecorder()

			srv.HandleWebTransport(rec, req)

			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.Empty(t, fake.received())
			assert.Empty(t, fake.ended())
		})
	}
}

// An admitted upgrade that fails anyway reports an end, which closes the
// session it counted at connect.
func TestServer_HandleWebTransport_WebSocketFailedUpgradeReportsEnd(t *testing.T) {
	fake := &fakeAuth{grant: testGrant(t, "", "**", 0)}
	srv := newWebSocketTestServer(t, fake)
	rec := httptest.NewRecorder()

	// Admitted, then refused by the upgrade: there is no Sec-WebSocket-Key.
	srv.HandleWebTransport(rec, webSocketUpgrade("https://relay.example/acme?jwt=a.b.c"))

	require.Len(t, fake.received(), 1)
	want := fake.received()[0]
	want.Event = auth.EventEnd
	want.Reason = endUpgradeFailed
	assert.Equal(t, []auth.Request{want}, fake.ended())
}

// A WebSocket session is admitted, served and reported as a WebTransport
// one is, under its own transport label.
func TestServer_HandleWebTransport_WebSocketSession(t *testing.T) {
	fake := &fakeAuth{grant: testGrant(t, "acme/**", "acme/**", 0)}
	srv := newWebSocketTestServer(t, fake)
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.HandleWebTransport))
	defer httpSrv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	served := metricSessionsTotal.WithLabelValues(auth.TransportWebSocket)
	closed := metricSessionsClosed.WithLabelValues(auth.TransportWebSocket, endClosed)
	servedBefore, closedBefore := testutil.ToFloat64(served), testutil.ToFloat64(closed)

	url := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/acme/app?jwt=a.b.c"
	sess, err := (&moqt.Dialer{}).Dial(ctx, url, moqt.NewTrackMux(0))
	require.NoError(t, err)

	require.Eventually(t, func() bool { return testutil.ToFloat64(served) == servedBefore+1 },
		5*time.Second, 5*time.Millisecond, "the session is counted under its transport")
	require.Len(t, fake.received(), 1)
	connect := fake.received()[0]
	assert.Equal(t, auth.TransportWebSocket, connect.Transport)
	assert.Equal(t, "/acme/app", connect.Path)
	assert.Equal(t, "jwt=a.b.c", connect.Query)

	require.NoError(t, sess.CloseWithError(moqt.NoError, "done"))

	require.Eventually(t, func() bool { return len(fake.ended()) == 1 }, 5*time.Second, 5*time.Millisecond,
		"the session's end is reported")
	end := fake.ended()[0]
	assert.Equal(t, connect.ID, end.ID)
	assert.Equal(t, auth.EventEnd, end.Event)
	assert.Equal(t, auth.TransportWebSocket, end.Transport)
	assert.Equal(t, endClosed, end.Reason)
	assert.Equal(t, closedBefore+1, testutil.ToFloat64(closed))
}

// HandleWebSocket is for a listener that takes WebSocket alone: nothing else
// gets as far as being checked.
func TestServer_HandleWebSocket_NotAnUpgrade(t *testing.T) {
	tests := map[string]struct {
		webSocket bool
		request   func() *http.Request
	}{
		"a plain GET": {
			webSocket: true,
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "https://relay.example/acme?jwt=a.b.c", nil)
			},
		},
		"a WebTransport CONNECT": {
			webSocket: true,
			request: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://relay.example/acme?jwt=a.b.c", nil)
				r.Method = http.MethodConnect
				return r
			},
		},
		"a WebSocket upgrade with WebSocket off": {
			webSocket: false,
			request:   func() *http.Request { return webSocketUpgrade("https://relay.example/acme?jwt=a.b.c") },
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fake := &fakeAuth{grant: testGrant(t, "", "**", 0)}
			srv := newWebSocketTestServer(t, fake)
			srv.Config.WebSocket = tt.webSocket
			rec := httptest.NewRecorder()

			srv.HandleWebSocket(rec, tt.request())

			assert.Equal(t, http.StatusUpgradeRequired, rec.Code)
			assert.Empty(t, fake.received())
		})
	}
}

// HandleWebSocket admits an upgrade as HandleWebTransport does.
func TestServer_HandleWebSocket_Admits(t *testing.T) {
	fake := &fakeAuth{grant: testGrant(t, "", "**", 0)}
	srv := newWebSocketTestServer(t, fake)
	rec := httptest.NewRecorder()

	// Admitted, then refused by the upgrade: there is no Sec-WebSocket-Key.
	srv.HandleWebSocket(rec, webSocketUpgrade("https://relay.example/acme?jwt=a.b.c"))

	require.Len(t, fake.received(), 1)
	assert.Equal(t, auth.TransportWebSocket, fake.received()[0].Transport)
	assert.Len(t, fake.ended(), 1, "the failed upgrade reports its end")
}

func TestRefusalReason(t *testing.T) {
	tests := map[string]struct {
		err  error
		want string
	}{
		"401":                  {err: auth.RefusedError{Status: http.StatusUnauthorized}, want: refusalRefused},
		"403":                  {err: auth.RefusedError{Status: http.StatusForbidden}, want: refusalRefused},
		"a wrapped refusal":    {err: fmt.Errorf("verify: %w", auth.RefusedError{Status: http.StatusUnauthorized}), want: refusalRefused},
		"could not be checked": {err: errors.New("no key set loaded yet"), want: refusalUnavailable},
		"no Authorize":         {err: errNoAuthorize, want: refusalUnavailable},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := refusalReason(tt.err)

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCheckWebSocketAddr(t *testing.T) {
	tests := map[string]struct {
		relayAddr string
		wsAddr    string
		wantErr   string
	}{
		"no listener":                    {relayAddr: ":4433", wsAddr: ""},
		"a port of its own":              {relayAddr: ":4433", wsAddr: ":443"},
		"the same port on another host":  {relayAddr: "127.0.0.1:4433", wsAddr: "192.0.2.1:4433"},
		"RELAY_ADDR's port":              {relayAddr: ":4433", wsAddr: ":4433", wantErr: "RELAY_ADDR's TCP port"},
		"RELAY_ADDR's port on one host":  {relayAddr: "127.0.0.1:4433", wsAddr: "127.0.0.1:4433", wantErr: "RELAY_ADDR's TCP port"},
		"RELAY_ADDR's port on all hosts": {relayAddr: "127.0.0.1:4433", wsAddr: ":4433", wantErr: "RELAY_ADDR's TCP port"},
		"all hosts under one of them":    {relayAddr: ":4433", wsAddr: "127.0.0.1:4433", wantErr: "RELAY_ADDR's TCP port"},
		"not an address":                 {relayAddr: ":4433", wsAddr: "443", wantErr: "invalid WS_TLS_ADDR"},
		"a RELAY_ADDR that is not one":   {relayAddr: "4433", wsAddr: ":443"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := checkWebSocketAddr(tt.relayAddr, tt.wsAddr)

			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestSessionTransport(t *testing.T) {
	internal := pendingAdmission()
	internal.decideInternal()
	tests := map[string]struct {
		admission *admission
		want      string
	}{
		"a dialed peer has no admission": {admission: nil, want: transportPeer},
		"an inbound peer has no connect request": {
			admission: decidedAdmission(nil, auth.Request{}), want: transportPeer,
		},
		"an internal client": {admission: internal, want: transportInternal},
		"a WebTransport client": {
			admission: decidedAdmission(nil, auth.Request{Transport: auth.TransportWebTransport}),
			want:      auth.TransportWebTransport,
		},
		"a WebSocket client": {
			admission: decidedAdmission(nil, auth.Request{Transport: auth.TransportWebSocket}),
			want:      auth.TransportWebSocket,
		},
		"a native-QUIC client": {
			admission: decidedAdmission(nil, auth.Request{Transport: auth.TransportQUIC}),
			want:      auth.TransportQUIC,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := sessionTransport(tt.admission)

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEnvBool(t *testing.T) {
	tests := map[string]struct {
		value string
		want  bool
	}{
		"1":             {value: "1", want: true},
		"true":          {value: "true", want: true},
		"any case":      {value: "TRUE", want: true},
		"yes":           {value: "yes", want: true},
		"on":            {value: " on ", want: true},
		"unset":         {value: ""},
		"0":             {value: "0"},
		"false":         {value: "false"},
		"anything else": {value: "enabled"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("QUMO_TEST_ENV_BOOL", tt.value)

			got := envBool("QUMO_TEST_ENV_BOOL")

			assert.Equal(t, tt.want, got)
		})
	}
}

// ListenAndServe returns with the first server to end, and Shutdown ends
// them all.
func TestServers(t *testing.T) {
	failed := errors.New("address already in use")
	running, failing := newMockServer(nil), newMockServer(failed)
	ss := servers{running, failing}

	err := ss.ListenAndServe()

	assert.ErrorIs(t, err, failed)
	<-running.listenCalled
	require.NoError(t, ss.Shutdown(context.Background()))
	<-running.shutdownCalled
	<-failing.shutdownCalled
}

// A panic in one server ends ListenAndServe with an error, so that the
// caller shuts the others down.
func TestServers_ListenAndServe_Panic(t *testing.T) {
	running := newMockServer(nil)
	ss := servers{running, &panicServer{}}

	err := ss.ListenAndServe()

	assert.ErrorContains(t, err, "panic in ListenAndServe: boom")
	require.NoError(t, ss.Shutdown(context.Background()))
	<-running.shutdownCalled
}
