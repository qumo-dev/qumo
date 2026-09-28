//go:build integration

// Package relay integration test for the per-announcement credential auth path
// (Server.authenticateAnnouncement). Tagged `integration` because it stands up
// a real QUIC/MOQT relay; run with `go test -tags=integration ./internal/relay/...`.
//
// This is the only coverage of the server-side ANNOUNCE gating: the credential
// client and meter are unit-tested separately, but the wiring that subscribes to
// a publisher's "auth" track, reads the JWT, introspects it, and registers the
// session with the meter only on success is exercised here end-to-end.
package relay

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubCredentialBackend impersonates the qumo control plane for the credential
// auth + metering path: it answers /v1/credentials/introspect with a configured
// validity and records every /v1/usage/events batch. It is the in-process peer
// of CredentialClient, scoped to the two endpoints the relay calls.
type stubCredentialBackend struct {
	srv *httptest.Server

	mu             sync.Mutex
	valid          bool
	introspectJWTs []string
	usageEventN    int
}

func newStubCredentialBackend(valid bool) *stubCredentialBackend {
	s := &stubCredentialBackend{valid: valid}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func (s *stubCredentialBackend) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/credentials/introspect":
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req struct {
			Token string `json:"token"`
		}
		_ = json.Unmarshal(body, &req)
		s.mu.Lock()
		s.introspectJWTs = append(s.introspectJWTs, req.Token)
		valid := s.valid
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"valid":            valid,
			"token_id":         "tok-test",
			"project_id":       "proj-test",
			"revalidate_after": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	case "/v1/usage/events":
		var batch []UsageEvent
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&batch)
		s.mu.Lock()
		s.usageEventN += len(batch)
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *stubCredentialBackend) Close() { s.srv.Close() }

func (s *stubCredentialBackend) introspectCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.introspectJWTs)
}

func (s *stubCredentialBackend) usageCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usageEventN
}

// startAuthRelay stands up a real QUIC/MOQT relay whose WebTransport (publisher)
// path requires per-announcement credential auth, backed by stub and a
// fast-ticking meter. Returns the relay's loopback address.
func startAuthRelay(t *testing.T, stub *stubCredentialBackend) (addr string, shutdown func()) {
	t.Helper()
	addr, _, shutdown = startAuthRelayWithPeers(t, stub, nil)
	return addr, shutdown
}

// startAuthRelayWithPeers is startAuthRelay with PEER_CIDRS set, returning the
// server so a test can inspect its routes.
func startAuthRelayWithPeers(t *testing.T, stub *stubCredentialBackend, peerCIDRs []netip.Prefix) (addr string, srv *Server, shutdown func()) {
	t.Helper()
	client := &CredentialClient{
		baseURL:    stub.srv.URL,
		authToken:  "relay-shared-secret",
		httpClient: stub.srv.Client(),
		cache:      map[string]cachedCredential{},
	}
	meter := newMeter(client)
	meter.interval = 100 * time.Millisecond
	return startRelay(t, relayAuth{client: client, meter: meter, peerCIDRs: peerCIDRs})
}

// relayAuth is how a test relay authenticates publishers: an introspection
// client with its meter, or a local verifier (no meter).
type relayAuth struct {
	client    *CredentialClient
	meter     *Meter
	verifier  *localVerifier
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
		credentialClient: auth.client,
		meter:            auth.meter,
		verifier:         auth.verifier,
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
// per-announcement credential gating. A valid credential must result in the
// session being registered with the meter (usage events flow); any failure
// (invalid credential, empty JWT, missing auth track) must reject the
// announcement with no usage events. Introspect is reached only when the relay
// actually reads a non-empty JWT from the auth track.
func TestServer_AuthenticateAnnouncement(t *testing.T) {
	const jwt = "header.payload.signature"
	cases := map[string]struct {
		jwt             string
		serveAuthTrack  bool
		introspectValid bool
		wantAccepted    bool
		wantIntrospect  bool
	}{
		"valid credential":   {jwt: jwt, serveAuthTrack: true, introspectValid: true, wantAccepted: true, wantIntrospect: true},
		"invalid credential": {jwt: jwt, serveAuthTrack: true, introspectValid: false, wantAccepted: false, wantIntrospect: true},
		"empty JWT":          {jwt: "", serveAuthTrack: true, introspectValid: true, wantAccepted: false, wantIntrospect: false},
		"missing auth track": {jwt: jwt, serveAuthTrack: false, introspectValid: true, wantAccepted: false, wantIntrospect: false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stub := newStubCredentialBackend(tc.introspectValid)
			t.Cleanup(stub.Close)

			addr, shutdown := startAuthRelay(t, stub)
			t.Cleanup(shutdown)

			_ = publishWithAuthTrack(t, addr, "/test-auth", tc.jwt, tc.serveAuthTrack)

			// Introspect is reached only when a non-empty JWT was read from the
			// auth track; missing-track and empty-JWT reject before it.
			if tc.wantIntrospect {
				require.Eventually(t, func() bool { return stub.introspectCount() > 0 },
					3*time.Second, 25*time.Millisecond,
					"introspect endpoint was not called")
			} else {
				// authenticateAnnouncement resolves within the 5s authCtx; with a
				// missing track or empty JWT it returns immediately. assert.Never
				// confirms it does not fire over a window long enough that a
				// delayed introspect would have — without a bare time.Sleep.
				assert.Never(t, func() bool { return stub.introspectCount() > 0 },
					1500*time.Millisecond, 25*time.Millisecond,
					"introspect must not be called when the auth track is absent or empty")
			}

			// meter.Register runs only on a successful authentication, so usage
			// events are the clean accept/reject signal: present iff accepted.
			if tc.wantAccepted {
				require.Eventually(t, func() bool { return stub.usageCount() > 0 },
					3*time.Second, 25*time.Millisecond,
					"no usage events reported for an accepted announcement")
			} else {
				assert.Never(t, func() bool { return stub.usageCount() > 0 },
					1500*time.Millisecond, 25*time.Millisecond,
					"no usage events should be reported for a rejected announcement")
			}
		})
	}
}

// TestServer_NativeQUICPeerTrust covers native-QUIC (ALPN moqt) publishers on a
// relay with credential auth on. Speaking the native protocol must not make a
// session a relay peer: unless it comes from a PEER_CIDRS network (or presents
// a verified mTLS client certificate), its announcements are authenticated
// exactly like a WebTransport client's.
func TestServer_NativeQUICPeerTrust(t *testing.T) {
	const jwt = "header.payload.signature"
	loopback := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	cases := map[string]struct {
		peerCIDRs       []netip.Prefix
		jwt             string
		serveAuthTrack  bool
		introspectValid bool
		wantRoute       bool
		wantIntrospect  bool
	}{
		"untrusted, no credential":      {serveAuthTrack: false, introspectValid: true, wantRoute: false, wantIntrospect: false},
		"untrusted, invalid credential": {jwt: jwt, serveAuthTrack: true, introspectValid: false, wantRoute: false, wantIntrospect: true},
		"untrusted, valid credential":   {jwt: jwt, serveAuthTrack: true, introspectValid: true, wantRoute: true, wantIntrospect: true},
		"trusted peer network":          {peerCIDRs: loopback, serveAuthTrack: false, introspectValid: false, wantRoute: true, wantIntrospect: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stub := newStubCredentialBackend(tc.introspectValid)
			t.Cleanup(stub.Close)

			addr, srv, shutdown := startAuthRelayWithPeers(t, stub, tc.peerCIDRs)
			t.Cleanup(shutdown)

			const path = moqt.BroadcastPath("/other-tenant/victim/live")
			_ = publishNativeWithAuthTrack(t, addr, path, tc.jwt, tc.serveAuthTrack)

			// TrackHandler returns NotFoundTrackHandler (non-nil) for an unknown
			// path; the announcement is nil unless the route was installed.
			routed := func() bool { ann, _ := srv.TrackMux.TrackHandler(path); return ann != nil }
			if tc.wantRoute {
				require.Eventually(t, routed, 3*time.Second, 25*time.Millisecond, "announcement should be routed")
			} else {
				assert.Never(t, routed, 1500*time.Millisecond, 25*time.Millisecond, "announcement must not be routed")
			}
			if tc.wantIntrospect {
				assert.Positive(t, stub.introspectCount(), "an untrusted native session must be authenticated")
			} else {
				assert.Zero(t, stub.introspectCount(), "no credential check expected")
			}
		})
	}
}

// TestServer_AuthenticateAnnouncement_Local is the end-to-end coverage of local
// verification (QUMO_RELAY_AUDIENCE set): a relay configured for a dev project
// admits a dev credential without calling the control plane, and refuses a
// managed-audience credential, a path outside the grant, and a forgery.
func TestServer_AuthenticateAnnouncement_Local(t *testing.T) {
	signer := newTestSigner(t, testKID)
	forger := newTestSigner(t, testKID)
	header := map[string]any{"alg": "EdDSA", "kid": testKID}
	claims := func(aud string) map[string]any {
		now := time.Now()
		return map[string]any{
			"jti": "jti-local", "iss": testIssuer, "aud": aud,
			"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
			"path_auth": map[string]any{"root": "tenant/project", "pub": "live"},
		}
	}
	const granted = moqt.BroadcastPath("/tenant/project/live")

	cases := map[string]struct {
		jwt       string
		path      moqt.BroadcastPath
		wantRoute bool
	}{
		"dev credential":              {jwt: signer.sign(t, header, claims(testDevAudience)), path: granted, wantRoute: true},
		"managed credential":          {jwt: signer.sign(t, header, claims(managedAudience)), path: granted},
		"path outside the grant":      {jwt: signer.sign(t, header, claims(testDevAudience)), path: "/tenant/other/live"},
		"signed by a key not in JWKS": {jwt: forger.sign(t, header, claims(testDevAudience)), path: granted},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			verifier := &localVerifier{
				keys:     &fakeSigningKeys{keys: map[string]ed25519.PublicKey{testKID: signer.pub}},
				issuer:   testIssuer,
				audience: testDevAudience,
				now:      time.Now,
			}
			addr, srv, shutdown := startRelay(t, relayAuth{verifier: verifier})
			t.Cleanup(shutdown)

			_ = publishWithAuthTrack(t, addr, tc.path, tc.jwt, true)

			routed := func() bool { ann, _ := srv.TrackMux.TrackHandler(tc.path); return ann != nil }
			if tc.wantRoute {
				require.Eventually(t, routed, 3*time.Second, 25*time.Millisecond, "announcement should be routed")
			} else {
				assert.Never(t, routed, 1500*time.Millisecond, 25*time.Millisecond, "announcement must not be routed")
			}
		})
	}
}
