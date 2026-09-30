//go:build integration

// Package relay integration test for the per-announcement credential auth path
// (Server.authenticateAnnouncement). Tagged `integration` because it stands up
// a real QUIC/MOQT relay; run with `go test -tags=integration ./internal/relay/...`.
//
// This is the only coverage of the server-side ANNOUNCE gating: the verifier
// and meter are unit-tested separately, but the wiring that subscribes to a
// publisher's "auth" track, reads the JWT, verifies it against the trusted
// signing keys, and registers the session with the meter only on success is
// exercised here end-to-end.
package relay

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/credential"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testKID is the kid of the app signing key the test relays trust.
const testKID = "kid-app"

// stubUsageBackend impersonates the qumo control plane's usage endpoint and
// counts every /v1/usage/events record.
type stubUsageBackend struct {
	srv *httptest.Server

	mu     sync.Mutex
	events []UsageEvent
}

func newStubUsageBackend() *stubUsageBackend {
	s := &stubUsageBackend{}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func (s *stubUsageBackend) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/usage/events" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var batch []UsageEvent
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&batch)
	s.mu.Lock()
	s.events = append(s.events, batch...)
	s.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

func (s *stubUsageBackend) Close() { s.srv.Close() }

func (s *stubUsageBackend) usage() []UsageEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]UsageEvent(nil), s.events...)
}

// appSigner is an app's Ed25519 signing key.
type appSigner struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newAppSigner(t *testing.T) appSigner {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	return appSigner{pub: pub, priv: priv}
}

// sign mints a publish credential for tenant/project/live the way an app does.
func (a appSigner) sign(t *testing.T) string {
	t.Helper()
	now := time.Now()
	header, err := json.Marshal(map[string]any{"alg": "EdDSA", "kid": testKID})
	require.NoError(t, err)
	claims, err := json.Marshal(map[string]any{
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
		"path_auth": map[string]any{"root": "tenant/project", "pub": "live"},
	})
	require.NoError(t, err)
	enc := base64.RawURLEncoding.EncodeToString
	input := enc(header) + "." + enc(claims)
	return input + "." + enc(ed25519.Sign(a.priv, []byte(input)))
}

// startAuthRelay stands up a relay that trusts signer's key and reports usage
// to usage with a fast-ticking meter.
func startAuthRelay(t *testing.T, signer appSigner, usage *stubUsageBackend, peerCIDRs []netip.Prefix) (addr string, srv *Server, shutdown func()) {
	t.Helper()
	meter := newMeter(&usageClient{
		baseURL:    usage.srv.URL,
		authToken:  "relay-shared-secret",
		httpClient: usage.srv.Client(),
	})
	meter.interval = 100 * time.Millisecond
	return startRelay(t, relayAuth{
		verifier:  credential.NewVerifier(credential.StaticKeys{testKID: signer.pub}),
		meter:     meter,
		peerCIDRs: peerCIDRs,
	})
}

// relayAuth is how a test relay authenticates publishers.
type relayAuth struct {
	verifier  *credential.Verifier
	meter     *Meter
	peerCIDRs []netip.Prefix
}

// startRelay stands up a real QUIC/MOQT relay whose WebTransport (publisher)
// path requires per-announcement credential auth as configured by auth.
func startRelay(t *testing.T, auth relayAuth) (addr string, srv *Server, shutdown func()) {
	t.Helper()
	certFile, keyFile := createTempCert(t)
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	require.NoError(t, err)

	quicCfg := &quic.Config{
		EnableDatagrams: true,
		KeepAlivePeriod: 5 * time.Second,
		MaxIdleTimeout:  30 * time.Second,
	}
	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{cert},
		// Advertise both ALPNs: "h3" for WebTransport (publishers/browsers) and
		// "moqt" for native-QUIC peers. The publisher dials via WebTransport.
		NextProtos: []string{"h3", moqt.NextProtoMOQ},
		MinVersion: tls.VersionTLS13,
	}
	dialerTLS := &tls.Config{
		NextProtos:         []string{moqt.NextProtoMOQ},
		InsecureSkipVerify: true, //nolint:gosec // test-only self-signed cert
		MinVersion:         tls.VersionTLS13,
	}

	addr = fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))
	// Wire the WebTransport path through the relay's HandleWebTransport (which
	// dispatches to s.Relay → requireAuth=true), exactly as the relay command
	// does. Without this the moqt.Server falls back to its default WT handler,
	// which dispatches to MOQServer.Handler (relayPeer, no auth).
	httpMux := http.NewServeMux()
	srv = &Server{
		MOQServer: &moqt.Server{
			Addr:               addr,
			TLSConfig:          serverTLS,
			QUICConfig:         quicCfg,
			WebTransportServer: moqt.NewWebTransportServer(httpMux),
		},
		MOQDialer: &moqt.Dialer{TLSConfig: dialerTLS, QUICConfig: quicCfg},
		Config: &Config{
			NodeID:    "relay-auth-test",
			Role:      "relay",
			PeerCIDRs: auth.peerCIDRs,
		},
		verifier: auth.verifier,
		meter:    auth.meter,
	}
	// Register the WebTransport route after construction (httpMux is a pointer,
	// so this reaches the WebTransportServer wired above). Same shape as the
	// relay command.
	httpMux.HandleFunc("/", srv.HandleWebTransport)

	go func() { _ = srv.ListenAndServe() }()

	meterCtx, meterCancel := context.WithCancel(context.Background())
	if auth.meter != nil {
		go auth.meter.Run(meterCtx)
	}

	// Wait until the relay is accepting QUIC/MOQT sessions before returning, so
	// the publisher's first dial lands on a live listener.
	require.Eventually(t, func() bool {
		probe := &moqt.Dialer{TLSConfig: dialerTLS, QUICConfig: quicCfg}
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		sess, derr := probe.Dial(ctx, peerURL(addr), moqt.NewTrackMux(0))
		if derr != nil {
			return false
		}
		_ = sess.CloseWithError(0, "probe")
		return true
	}, 5*time.Second, 50*time.Millisecond, "auth relay never became reachable")

	return addr, srv, func() {
		meterCancel()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}
}

// publishWithAuthTrack announces broadcastPath on the relay and serves an "auth"
// track whose first (only) group carries jwt as the raw frame body. When
// serveAuthTrack is false the publisher announces without any auth track at all
// (exercising the missing-track rejection path); when jwt is "" the auth track
// is served but empty (exercising the empty-JWT rejection path).
func publishWithAuthTrack(t *testing.T, addr string, broadcastPath moqt.BroadcastPath, jwt string, serveAuthTrack bool) *moqt.Session {
	t.Helper()
	return publishWithAuthTrackOver(t, "https://"+addr, nil, broadcastPath, jwt, serveAuthTrack)
}

// publishNativeWithAuthTrack is publishWithAuthTrack over native QUIC (ALPN
// moqt) — the transport the relay used to treat as an unconditional peer.
func publishNativeWithAuthTrack(t *testing.T, addr string, broadcastPath moqt.BroadcastPath, jwt string, serveAuthTrack bool) *moqt.Session {
	t.Helper()
	return publishWithAuthTrackOver(t, peerURL(addr), []string{moqt.NextProtoMOQ}, broadcastPath, jwt, serveAuthTrack)
}

func publishWithAuthTrackOver(t *testing.T, url string, nextProtos []string, broadcastPath moqt.BroadcastPath, jwt string, serveAuthTrack bool) *moqt.Session {
	t.Helper()
	dialerTLS := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // test-only self-signed cert
		MinVersion:         tls.VersionTLS13,
		NextProtos:         nextProtos,
	}
	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}

	ctx := context.Background()
	mux := moqt.NewTrackMux(0)
	sess, err := (&moqt.Dialer{TLSConfig: dialerTLS, QUICConfig: quicCfg}).Dial(ctx, url, mux)
	require.NoError(t, err)

	ann, endAnn := moqt.NewAnnouncement(ctx, broadcastPath)
	broadcast := moqt.NewBroadcast()
	if serveAuthTrack {
		token := jwt
		broadcast.Register(authTrackName, moqt.TrackHandlerFunc(func(tw *moqt.TrackWriter) {
			g, err := tw.OpenGroup(ctx)
			if err != nil {
				return
			}
			if token != "" {
				f := moqt.NewFrame(len(token))
				_, _ = f.Write([]byte(token))
				_ = g.WriteFrame(f)
			}
			_ = g.Close()
		}))
	}
	mux.Announce(ann, broadcast)
	// Retract on test end so the announcement does not outlive the session.
	t.Cleanup(func() {
		endAnn()
		_ = sess.CloseWithError(moqt.NoError, "test done")
	})
	return sess
}

// TestServer_AuthenticateAnnouncement is the end-to-end coverage of the relay's
// per-announcement credential gating. An app-signed credential from a trusted
// key must route the announcement and register the session with the meter
// under its kid; any failure (path outside the grant, untrusted key, empty JWT,
// missing auth track) must leave it unrouted with no usage events.
func TestServer_AuthenticateAnnouncement(t *testing.T) {
	const granted = moqt.BroadcastPath("/tenant/project/live")
	signer := newAppSigner(t)
	forger := newAppSigner(t)

	cases := map[string]struct {
		jwt            string
		path           moqt.BroadcastPath
		serveAuthTrack bool
		wantAccepted   bool
	}{
		"trusted key":            {jwt: signer.sign(t), path: granted, serveAuthTrack: true, wantAccepted: true},
		"path outside the grant": {jwt: signer.sign(t), path: "/tenant/other/live", serveAuthTrack: true},
		"untrusted key":          {jwt: forger.sign(t), path: granted, serveAuthTrack: true},
		"empty JWT":              {jwt: "", path: granted, serveAuthTrack: true},
		"missing auth track":     {jwt: signer.sign(t), path: granted},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			usage := newStubUsageBackend()
			t.Cleanup(usage.Close)
			addr, srv, shutdown := startAuthRelay(t, signer, usage, nil)
			t.Cleanup(shutdown)

			_ = publishWithAuthTrack(t, addr, tc.path, tc.jwt, tc.serveAuthTrack)

			routed := func() bool { ann, _ := srv.TrackMux.TrackHandler(tc.path); return ann != nil }
			if !tc.wantAccepted {
				// assert.Never covers a window long enough that a delayed
				// admission would have shown, without a bare time.Sleep.
				assert.Never(t, func() bool { return routed() || len(usage.usage()) > 0 },
					1500*time.Millisecond, 25*time.Millisecond,
					"a rejected announcement must be neither routed nor metered")
				return
			}
			require.Eventually(t, routed, 3*time.Second, 25*time.Millisecond, "announcement should be routed")
			require.Eventually(t, func() bool { return len(usage.usage()) > 0 },
				3*time.Second, 25*time.Millisecond, "no usage events reported for an accepted announcement")
			assert.Equal(t, testKID, usage.usage()[0].KeyID)
		})
	}
}

// TestServer_NativeQUICPeerTrust covers native-QUIC (ALPN moqt) publishers on a
// relay with credential auth on. Speaking the native protocol must not make a
// session a relay peer: unless it comes from a PEER_CIDRS network (or presents
// a verified mTLS client certificate), its announcements are authenticated
// exactly like a WebTransport client's.
func TestServer_NativeQUICPeerTrust(t *testing.T) {
	const path = moqt.BroadcastPath("/tenant/project/live")
	signer := newAppSigner(t)
	forger := newAppSigner(t)
	loopback := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}

	cases := map[string]struct {
		peerCIDRs      []netip.Prefix
		jwt            string
		serveAuthTrack bool
		wantRoute      bool
		wantMetered    bool
	}{
		"untrusted, no credential":      {serveAuthTrack: false},
		"untrusted, invalid credential": {jwt: forger.sign(t), serveAuthTrack: true},
		"untrusted, valid credential":   {jwt: signer.sign(t), serveAuthTrack: true, wantRoute: true, wantMetered: true},
		"trusted peer network":          {peerCIDRs: loopback, serveAuthTrack: false, wantRoute: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			usage := newStubUsageBackend()
			t.Cleanup(usage.Close)
			addr, srv, shutdown := startAuthRelay(t, signer, usage, tc.peerCIDRs)
			t.Cleanup(shutdown)

			_ = publishNativeWithAuthTrack(t, addr, path, tc.jwt, tc.serveAuthTrack)

			// TrackHandler returns NotFoundTrackHandler (non-nil) for an unknown
			// path; the announcement is nil unless the route was installed.
			routed := func() bool { ann, _ := srv.TrackMux.TrackHandler(path); return ann != nil }
			if !tc.wantRoute {
				assert.Never(t, routed, 1500*time.Millisecond, 25*time.Millisecond, "announcement must not be routed")
				return
			}
			require.Eventually(t, routed, 3*time.Second, 25*time.Millisecond, "announcement should be routed")
			if tc.wantMetered {
				require.Eventually(t, func() bool { return len(usage.usage()) > 0 },
					3*time.Second, 25*time.Millisecond, "an authenticated native session must be metered")
			} else {
				assert.Never(t, func() bool { return len(usage.usage()) > 0 },
					500*time.Millisecond, 25*time.Millisecond, "a trusted peer is not authenticated, so not metered")
			}
		})
	}
}
