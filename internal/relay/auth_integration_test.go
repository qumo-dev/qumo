//go:build integration

// Integration tests for session admission (auth.go) on a real QUIC/MOQT relay:
// the auth server is asked at connect, a refused WebTransport client never gets
// a session, and an admitted one may announce only what its grant covers. Run
// with `go test -tags=integration ./internal/relay/...`.
package relay

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startAuthRelay stands up a real relay that admits client sessions through
// auth, wired the way the relay command does (WebTransport through
// HandleWebTransport). It returns the relay's loopback address and the server.
func startAuthRelay(t *testing.T, auth *sessionAuth, peerCIDRs []netip.Prefix) (string, *Server) {
	t.Helper()
	certFile, keyFile := createTempCert(t)
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	require.NoError(t, err)

	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}
	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h3", moqt.NextProtoMOQ},
		MinVersion:   tls.VersionTLS13,
	}
	dialerTLS := &tls.Config{
		NextProtos:         []string{moqt.NextProtoMOQ},
		InsecureSkipVerify: true, //nolint:gosec // test-only self-signed cert
		MinVersion:         tls.VersionTLS13,
	}

	addr := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))
	httpMux := http.NewServeMux()
	srv := &Server{
		MOQServer: &moqt.Server{
			Addr:               addr,
			TLSConfig:          serverTLS,
			QUICConfig:         quicCfg,
			WebTransportServer: moqt.NewWebTransportServer(httpMux),
		},
		MOQDialer: &moqt.Dialer{TLSConfig: dialerTLS, QUICConfig: quicCfg},
		Config:    &Config{NodeID: "relay-auth-test", Role: "relay", PeerCIDRs: peerCIDRs},
		auth:      auth,
	}
	httpMux.HandleFunc("/", srv.HandleWebTransport)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	// Wait for the listener. The probe is a native-QUIC session from loopback,
	// so it may be refused; any SETUP exchange, refused or not, proves the
	// relay is up.
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		sess, err := (&moqt.Dialer{TLSConfig: dialerTLS, QUICConfig: quicCfg}).Dial(ctx, peerURL(addr), moqt.NewTrackMux(0))
		if err != nil {
			return false
		}
		_ = sess.CloseWithError(moqt.NoError, "probe")
		return true
	}, 5*time.Second, 50*time.Millisecond, "relay never became reachable")
	return addr, srv
}

// announceOver dials url and announces path from the new session. It returns
// the dial error, so a test can assert on a refused upgrade.
func announceOver(t *testing.T, url string, nextProtos []string, path moqt.BroadcastPath) error {
	t.Helper()
	dialerTLS := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // test-only self-signed cert
		MinVersion:         tls.VersionTLS13,
		NextProtos:         nextProtos,
	}
	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	mux := moqt.NewTrackMux(0)
	sess, err := (&moqt.Dialer{TLSConfig: dialerTLS, QUICConfig: quicCfg}).Dial(ctx, url, mux)
	if err != nil {
		return err
	}
	ann, endAnn := moqt.NewAnnouncement(context.Background(), path)
	mux.Announce(ann, moqt.NewBroadcast())
	t.Cleanup(func() {
		endAnn()
		_ = sess.CloseWithError(moqt.NoError, "test done")
	})
	return nil
}

// routed reports whether the relay installed a route for path.
func routed(srv *Server, path moqt.BroadcastPath) func() bool {
	return func() bool {
		ann, _ := srv.TrackMux.TrackHandler(path)
		return ann != nil
	}
}

func TestServer_SessionAuth_WebTransport(t *testing.T) {
	const path = moqt.BroadcastPath("/acme/app/live")
	tests := map[string]struct {
		server    *fakeAuthServer
		wantDial  bool
		wantRoute bool
	}{
		"admitted, path granted": {
			server:    &fakeAuthServer{body: `{"publish":["acme/app/**"]}`},
			wantDial:  true,
			wantRoute: true,
		},
		"admitted, path not granted": {
			server:   &fakeAuthServer{body: `{"publish":["acme/other/**"],"subscribe":["acme/**"]}`},
			wantDial: true,
		},
		"401":               {server: &fakeAuthServer{status: http.StatusUnauthorized}},
		"403":               {server: &fakeAuthServer{status: http.StatusForbidden}},
		"auth server error": {server: &fakeAuthServer{status: http.StatusInternalServerError}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			auth := tt.server.start(t)
			addr, srv := startAuthRelay(t, auth, nil)
			probes := len(tt.server.received()) // startAuthRelay's readiness probes

			err := announceOver(t, "https://"+addr+"/acme/app?jwt=header.payload.signature", nil, path)

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
			reqs := tt.server.received()[probes:]
			require.Len(t, reqs, 1, "one connect request per session")
			assert.Equal(t, eventConnect, reqs[0].Event)
			assert.Equal(t, transportWebTransport, reqs[0].Transport)
			assert.Equal(t, "/acme/app", reqs[0].Path)
			assert.Equal(t, "jwt=header.payload.signature", reqs[0].Query, "the credential is forwarded unparsed")
			assert.Len(t, reqs[0].ID, 32)
		})
	}
}

func TestServer_SessionAuth_NativeQUIC(t *testing.T) {
	const path = moqt.BroadcastPath("/acme/app/live")
	loopback := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	tests := map[string]struct {
		server       *fakeAuthServer
		peerCIDRs    []netip.Prefix
		wantRoute    bool
		wantRequests bool
	}{
		"untrusted, admitted": {
			server:       &fakeAuthServer{body: `{"publish":["acme/**"]}`},
			wantRoute:    true,
			wantRequests: true,
		},
		"untrusted, refused": {
			server:       &fakeAuthServer{status: http.StatusUnauthorized},
			wantRequests: true,
		},
		"trusted peer network": {
			server:    &fakeAuthServer{status: http.StatusUnauthorized},
			peerCIDRs: loopback,
			wantRoute: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			auth := tt.server.start(t)
			addr, srv := startAuthRelay(t, auth, tt.peerCIDRs)
			probes := len(tt.server.received()) // startAuthRelay's readiness probes

			// A refused native session still completes SETUP; the relay closes it
			// right after, so the dial itself may succeed either way.
			_ = announceOver(t, peerURL(addr)+"/acme", []string{moqt.NextProtoMOQ}, path)

			if tt.wantRoute {
				require.Eventually(t, routed(srv, path), 3*time.Second, 25*time.Millisecond)
			} else {
				assert.Never(t, routed(srv, path), 1500*time.Millisecond, 25*time.Millisecond)
			}
			reqs := tt.server.received()[probes:]
			if !tt.wantRequests {
				assert.Empty(t, reqs, "a trusted peer is never asked")
				return
			}
			require.NotEmpty(t, reqs)
			assert.Equal(t, transportQUIC, reqs[len(reqs)-1].Transport)
			assert.Equal(t, "/acme", reqs[len(reqs)-1].Path)
		})
	}
}

func TestServer_SessionAuth_Public(t *testing.T) {
	patterns, err := parsePatterns([]string{"anon/**"})
	require.NoError(t, err)
	auth := &sessionAuth{public: &grant{publish: patterns, subscribe: patterns}}
	addr, srv := startAuthRelay(t, auth, nil)

	t.Run("anonymous session publishes under the grant", func(t *testing.T) {
		err := announceOver(t, "https://"+addr+"/", nil, "/anon/room")

		require.NoError(t, err)
		require.Eventually(t, routed(srv, "/anon/room"), 3*time.Second, 25*time.Millisecond)
	})
	t.Run("outside the grant is not routed", func(t *testing.T) {
		err := announceOver(t, "https://"+addr+"/", nil, "/private/room")

		require.NoError(t, err)
		assert.Never(t, routed(srv, "/private/room"), 1500*time.Millisecond, 25*time.Millisecond)
	})
	t.Run("a session presenting a credential is refused", func(t *testing.T) {
		err := announceOver(t, "https://"+addr+"/?jwt=header.payload.signature", nil, "/anon/other")

		require.Error(t, err)
	})
}
