package relay

import (
	"context"
	"errors"
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

func TestServer_HandleWebTransport_WebSocketRefused(t *testing.T) {
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
			srv := newWebSocketTestServer(t, fake)
			rec := httptest.NewRecorder()

			srv.HandleWebTransport(rec, webSocketUpgrade("https://relay.example/acme/app?jwt=a.b.c"))

			assert.Equal(t, tt.wantStatus, rec.Code)
			got := fake.received()
			require.Len(t, got, 1, "an upgrade is checked once")
			assert.Equal(t, auth.EventConnect, got[0].Event)
			assert.Equal(t, auth.TransportWebSocket, got[0].Transport)
			assert.Equal(t, "/acme/app", got[0].Path)
			assert.Equal(t, "jwt=a.b.c", got[0].Query, "the credential is forwarded unparsed")
			assert.Equal(t, "192.0.2.1:5000", got[0].Remote)
			assert.Len(t, got[0].ID, 32)
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

func TestIsWebSocketUpgrade(t *testing.T) {
	tests := map[string]struct {
		method string
		header http.Header
		want   bool
	}{
		"upgrade": {
			method: http.MethodGet,
			header: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}},
			want:   true,
		},
		"tokens among others, in any case": {
			method: http.MethodGet,
			header: http.Header{"Connection": {"keep-alive, UPGRADE"}, "Upgrade": {"WebSocket"}},
			want:   true,
		},
		"plain request":                {method: http.MethodGet, header: http.Header{}},
		"upgrade to something else":    {method: http.MethodGet, header: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"h2c"}}},
		"without the Connection token": {method: http.MethodGet, header: http.Header{"Upgrade": {"websocket"}}},
		"a WebTransport CONNECT":       {method: http.MethodConnect, header: http.Header{}},
		"not a GET": {
			method: http.MethodPost,
			header: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := isWebSocketUpgrade(&http.Request{Method: tt.method, Header: tt.header})

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestOffersSubprotocol(t *testing.T) {
	tests := map[string]struct {
		offered []string
		want    bool
	}{
		"offered":                               {offered: []string{"qmux-02.moq-lite-05"}, want: true},
		"among others in one value":             {offered: []string{"qmux-01.moq-lite-05, qmux-02.moq-lite-05"}, want: true},
		"among others in several values":        {offered: []string{"webtransport", "qmux-02.moq-lite-05"}, want: true},
		"nothing offered":                       {offered: nil},
		"an empty value":                        {offered: []string{""}},
		"another draft":                         {offered: []string{"qmux-01.moq-lite-05"}},
		"the draft alone":                       {offered: []string{"qmux-02"}},
		"a prefix":                              {offered: []string{"qmux-02.moq-lite"}},
		"another case: they are case-sensitive": {offered: []string{"QMUX-02.MOQ-LITE-05"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := &http.Request{Header: http.Header{"Sec-Websocket-Protocol": tt.offered}}

			got := offersSubprotocol(r, "qmux-02.moq-lite-05")

			assert.Equal(t, tt.want, got)
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
