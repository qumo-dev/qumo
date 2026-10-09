//go:build integration

// Black-box tests of the relay's WebSocket path (MoQ over QMux), on a real
// relay: a WebSocket session is admitted by the same checks as a
// WebTransport one, and exchanges media with sessions on the other
// transports through the shared TrackMux.
package integration

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/internal/relay"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withWebSocket makes a relay take WebSocket upgrades, as WS_ENABLE does.
func withWebSocket(s *relay.Server) {
	s.Config.WebSocket = true
}

// startWebSocket serves srv's client endpoint over HTTP/1.1 on a TCP port,
// as the relay command does, and returns its ws:// URL without a path.
func startWebSocket(t *testing.T, srv *relay.Server) string {
	t.Helper()
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.HandleWebTransport))
	t.Cleanup(httpSrv.Close)
	return "ws" + strings.TrimPrefix(httpSrv.URL, "http")
}

func TestRelay_SessionAuth_WebSocket(t *testing.T) {
	const path = moqt.BroadcastPath("/acme/app/live")
	tests := map[string]struct {
		server    *fakeAuth
		wantDial  bool
		wantRoute bool
	}{
		"admitted, path granted": {
			server:    &fakeAuth{grant: testGrant(t, "acme/app/**", "", 0)},
			wantDial:  true,
			wantRoute: true,
		},
		"admitted, path not granted": {
			server:   &fakeAuth{grant: testGrant(t, "acme/other/**", "acme/**", 0)},
			wantDial: true,
		},
		"401":              {server: &fakeAuth{err: auth.RefusedError{Status: http.StatusUnauthorized}}},
		"403":              {server: &fakeAuth{err: auth.RefusedError{Status: http.StatusForbidden}}},
		"can't be checked": {server: &fakeAuth{err: errors.New("no key set loaded yet")}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, srv := startAuthRelay(t, tt.server.authorize, nil, withWebSocket)
			ws := startWebSocket(t, srv)

			err := announceOver(t, ws+"/acme/app?jwt=header.payload.signature", nil, nil, path)

			if !tt.wantDial {
				require.Error(t, err, "a refused client must not get a session")
				assert.Never(t, routed(srv, path), 500*time.Millisecond, 25*time.Millisecond)
			} else {
				require.NoError(t, err)
				if tt.wantRoute {
					require.Eventually(t, routed(srv, path), 3*time.Second, 25*time.Millisecond)
				} else {
					assert.Never(t, routed(srv, path), 1500*time.Millisecond, 25*time.Millisecond)
				}
			}
			// startAuthRelay's readiness probes are native-QUIC sessions;
			// this session is the only WebSocket one.
			var reqs []auth.Request
			for _, req := range tt.server.received() {
				if req.Transport == auth.TransportWebSocket {
					reqs = append(reqs, req)
				}
			}
			require.Len(t, reqs, 1, "one connect request per session")
			assert.Equal(t, auth.EventConnect, reqs[0].Event)
			assert.Equal(t, "/acme/app", reqs[0].Path)
			assert.Equal(t, "jwt=header.payload.signature", reqs[0].Query, "the credential is forwarded unparsed")
			assert.Len(t, reqs[0].ID, 32)
		})
	}
}

// A subscription over WebSocket is checked against the session's grant as
// on the other transports.
func TestRelay_SubscribeAuth_WebSocket(t *testing.T) {
	server := &fakeAuth{grant: testGrant(t, "acme/**", "acme/app/**", 0)}
	addr, srv := startAuthRelay(t, server.authorize, nil, withWebSocket)
	url := startWebSocket(t, srv) + "/?jwt=a.b.c"
	publishOver(t, srv, "https://"+addr+"/?jwt=a.b.c", "/acme/app/live", "/acme/apple/live")

	t.Run("inside the grant", func(t *testing.T) {
		err := subscribe(t, url, nil, "/acme/app/live")

		assert.NoError(t, err)
	})
	t.Run("outside the grant looks like a missing path", func(t *testing.T) {
		refused := subscribe(t, url, nil, "/acme/apple/live")
		missing := subscribe(t, url, nil, "/acme/app/none")

		require.Error(t, refused)
		require.Error(t, missing)
		assert.Equal(t, missing.Error(), refused.Error())
	})
}

// A publisher on one transport and a subscriber on another exchange media
// through one relay: sessions on every transport share the TrackMux.
func TestRelay_WebSocket_CrossTransport(t *testing.T) {
	server := &fakeAuth{grant: testGrant(t, "**", "**", 0)}
	addr, srv := startAuthRelay(t, server.authorize, nil, withWebSocket)
	urls := map[string]string{
		"websocket":    startWebSocket(t, srv) + "/?jwt=a.b.c",
		"webtransport": "https://" + addr + "/?jwt=a.b.c",
		"native QUIC":  nativeURL(addr) + "/?jwt=a.b.c",
	}
	tests := map[string]struct {
		publisher  string
		subscriber string
	}{
		"WebSocket to WebTransport": {publisher: "websocket", subscriber: "webtransport"},
		"WebSocket to native QUIC":  {publisher: "websocket", subscriber: "native QUIC"},
		"WebTransport to WebSocket": {publisher: "webtransport", subscriber: "websocket"},
		"native QUIC to WebSocket":  {publisher: "native QUIC", subscriber: "websocket"},
		"WebSocket to WebSocket":    {publisher: "websocket", subscriber: "websocket"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// One path per case: each publisher lives until the test ends.
			path := moqt.BroadcastPath("/cross/" + strings.ReplaceAll(strings.ToLower(name), " ", "-"))
			publishOver(t, srv, urls[tt.publisher], path)

			err := subscribe(t, urls[tt.subscriber], nil, path)

			assert.NoError(t, err)
		})
	}
}
