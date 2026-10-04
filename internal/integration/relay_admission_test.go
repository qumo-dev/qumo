//go:build integration

// Black-box tests of the relay's session admission, through its public API
// (relay.Server with an Authorize function) on a real QUIC/MOQT relay: the
// auth server is asked at connect, a refused WebTransport client never gets a
// session, and an admitted one may announce only what its grant covers. Run
// with `go test -tags=integration ./internal/integration/...`.
package integration

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/internal/relay"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startAuthRelay stands up a real relay that admits client sessions through
// auth, wired the way the relay command does (WebTransport through
// HandleWebTransport). peerCA, when set, plays CA_FILE: a client certificate
// it verifies makes the session a trusted peer. opts adjust the Server before
// it starts, such as setting End. It returns the relay's loopback address and
// the server.
func startAuthRelay(t *testing.T, authorize func(context.Context, auth.Request) (*auth.Grant, error), peerCA *tls.Certificate, opts ...func(*relay.Server)) (string, *relay.Server) {
	t.Helper()
	cert := loadTempCert(t)

	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}
	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h3", moqt.NextProtoMOQ},
		MinVersion:   tls.VersionTLS13,
	}
	if peerCA != nil {
		pool := x509.NewCertPool()
		pool.AddCert(peerCA.Leaf)
		serverTLS.ClientAuth = tls.VerifyClientCertIfGiven
		serverTLS.ClientCAs = pool
	}
	dialerTLS := &tls.Config{
		NextProtos:         []string{moqt.NextProtoMOQ},
		InsecureSkipVerify: true, //nolint:gosec // test-only self-signed cert
		MinVersion:         tls.VersionTLS13,
	}

	addr := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))
	httpMux := http.NewServeMux()
	srv := &relay.Server{
		MOQServer: &moqt.Server{
			Addr:               addr,
			TLSConfig:          serverTLS,
			QUICConfig:         quicCfg,
			WebTransportServer: moqt.NewWebTransportServer(httpMux),
		},
		MOQDialer: &moqt.Dialer{TLSConfig: dialerTLS, QUICConfig: quicCfg},
		Config:    &relay.Config{NodeID: "relay-auth-test", Role: "relay"},
		// A per-relay hop id, as the relay command uses: with two relays
		// peered, it stops an announcement looping between them.
		TrackMux:  moqt.NewTrackMux(moqt.NewHopID()),
		Authorize: authorize,
	}
	for _, opt := range opts {
		opt(srv)
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
		sess, err := (&moqt.Dialer{TLSConfig: dialerTLS, QUICConfig: quicCfg}).Dial(ctx, nativeURL(addr), moqt.NewTrackMux(0))
		if err != nil {
			return false
		}
		_ = sess.CloseWithError(moqt.NoError, "probe")
		return true
	}, 5*time.Second, 50*time.Millisecond, "relay never became reachable")
	return addr, srv
}

// loadTempCert returns a fresh self-signed certificate. It has no key usage
// restrictions, so it serves as a server certificate, a client certificate,
// and its own CA.
func loadTempCert(t *testing.T) tls.Certificate {
	t.Helper()
	certFile, keyFile := createTempCert(t)
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	require.NoError(t, err)
	return cert
}

// announceOver dials url, presenting clientCert when set, and announces path
// from the new session. It returns the dial error, so a test can assert on a
// refused upgrade or handshake.
func announceOver(t *testing.T, url string, nextProtos []string, clientCert *tls.Certificate, path moqt.BroadcastPath) error {
	t.Helper()
	dialerTLS := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // test-only self-signed cert
		MinVersion:         tls.VersionTLS13,
		NextProtos:         nextProtos,
	}
	if clientCert != nil {
		dialerTLS.Certificates = []tls.Certificate{*clientCert}
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
func routed(srv *relay.Server, path moqt.BroadcastPath) func() bool {
	return func() bool {
		ann, _ := srv.TrackMux.TrackHandler(path)
		return ann != nil
	}
}

func TestRelay_SessionAuth_WebTransport(t *testing.T) {
	const path = moqt.BroadcastPath("/acme/app/live")
	tests := map[string]struct {
		server    *fakeAuth
		wantDial  bool
		wantRoute bool
	}{
		"admitted, path granted": {
			server:    &fakeAuth{body: `{"publish":["acme/app/**"]}`},
			wantDial:  true,
			wantRoute: true,
		},
		"admitted, path not granted": {
			server:   &fakeAuth{body: `{"publish":["acme/other/**"],"subscribe":["acme/**"]}`},
			wantDial: true,
		},
		"401":               {server: &fakeAuth{err: auth.RefusedError{Status: http.StatusUnauthorized}}},
		"403":               {server: &fakeAuth{err: auth.RefusedError{Status: http.StatusForbidden}}},
		"auth server error": {server: &fakeAuth{err: errors.New("auth server unavailable")}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			addr, srv := startAuthRelay(t, tt.server.authorize, nil)

			err := announceOver(t, "https://"+addr+"/acme/app?jwt=header.payload.signature", nil, nil, path)

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
			// startAuthRelay's readiness probes are native-QUIC sessions whose
			// requests can land after it returns; this session is the only
			// WebTransport one.
			var reqs []auth.Request
			for _, req := range tt.server.received() {
				if req.Transport == auth.TransportWebTransport {
					reqs = append(reqs, req)
				}
			}
			require.Len(t, reqs, 1, "one connect request per session")
			assert.Equal(t, auth.EventConnect, reqs[0].Event)
			assert.Equal(t, auth.TransportWebTransport, reqs[0].Transport)
			assert.Equal(t, "/acme/app", reqs[0].Path)
			assert.Equal(t, "jwt=header.payload.signature", reqs[0].Query, "the credential is forwarded unparsed")
			assert.Len(t, reqs[0].ID, 32)
		})
	}
}

func TestRelay_SessionAuth_NativeQUIC(t *testing.T) {
	const path = moqt.BroadcastPath("/acme/app/live")
	peerCert := loadTempCert(t)
	otherCert := loadTempCert(t)
	tests := map[string]struct {
		server       *fakeAuth
		peerCA       *tls.Certificate // the relay's CA_FILE; nil means unset
		clientCert   *tls.Certificate // what the dialing session presents
		wantRoute    bool
		wantRequests bool
	}{
		"untrusted, admitted": {
			server:       &fakeAuth{body: `{"publish":["acme/**"]}`},
			wantRoute:    true,
			wantRequests: true,
		},
		"untrusted, refused": {
			server:       &fakeAuth{err: auth.RefusedError{Status: http.StatusUnauthorized}},
			wantRequests: true,
		},
		"peer certificate verified by CA_FILE": {
			server:     &fakeAuth{err: auth.RefusedError{Status: http.StatusUnauthorized}},
			peerCA:     &peerCert,
			clientCert: &peerCert,
			wantRoute:  true,
		},
		"certificate from another CA fails the handshake": {
			server:     &fakeAuth{err: auth.RefusedError{Status: http.StatusUnauthorized}},
			peerCA:     &peerCert,
			clientCert: &otherCert,
		},
		"certificate without CA_FILE is not a peer": {
			server:       &fakeAuth{err: auth.RefusedError{Status: http.StatusUnauthorized}},
			clientCert:   &peerCert,
			wantRequests: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			addr, srv := startAuthRelay(t, tt.server.authorize, tt.peerCA)

			// A refused native session still completes SETUP; the relay closes it
			// right after, so the dial itself may succeed either way.
			_ = announceOver(t, nativeURL(addr)+"/acme?jwt=header.payload.signature", []string{moqt.NextProtoMOQ}, tt.clientCert, path)

			if tt.wantRoute {
				require.Eventually(t, routed(srv, path), 3*time.Second, 25*time.Millisecond)
			} else {
				assert.Never(t, routed(srv, path), 1500*time.Millisecond, 25*time.Millisecond)
			}
			// startAuthRelay's readiness probes dial "/", and their requests can
			// land after it returns; this session is the only one at /acme.
			var reqs []auth.Request
			for _, req := range tt.server.received() {
				if req.Path == "/acme" {
					reqs = append(reqs, req)
				}
			}
			if !tt.wantRequests {
				assert.Empty(t, reqs, "a trusted peer, or a failed handshake, is never asked")
				return
			}
			require.NotEmpty(t, reqs)
			assert.Equal(t, auth.TransportQUIC, reqs[len(reqs)-1].Transport)
			assert.Equal(t, "/acme", reqs[len(reqs)-1].Path)
			assert.Equal(t, "jwt=header.payload.signature", reqs[len(reqs)-1].Query, "native QUIC carries the credential too")
		})
	}
}
