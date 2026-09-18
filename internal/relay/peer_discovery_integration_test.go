//go:build integration

// Package relay integration test. Tagged `integration` so it stays out of the
// default `go test ./...` unit run (it stands up real QUIC relays); run it with
// `go test -tags=integration ./internal/relay/...`.
package relay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/require"
)

// freeUDPPort returns an ephemeral UDP port on loopback. There is a small TOCTOU
// window between closing the probe socket and the relay binding it — acceptable
// for a local in-process test.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	require.NoError(t, err)
	c, err := net.ListenUDP("udp", addr)
	require.NoError(t, err)
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// TestPeerDiscovery_EdgeConnectsToHubViaUpstreamAddr is an in-process
// integration test for the UPSTREAM_ADDR path: a real edge relay, configured
// with UpstreamAddr pointing at a real hub relay, completes a QUIC/MOQT
// handshake to it. No Docker or Nomad required — this complements the manual
// docker/nomad simulation and would catch regressions in the dial loop (e.g.
// the #93 class, where an edge filtered out all hubs).
func TestPeerDiscovery_EdgeConnectsToHubViaUpstreamAddr(t *testing.T) {
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
		NextProtos:   []string{moqt.NextProtoMOQ},
		MinVersion:   tls.VersionTLS13,
	}
	dialerTLS := &tls.Config{
		NextProtos:         []string{moqt.NextProtoMOQ},
		InsecureSkipVerify: true, //nolint:gosec // test only
		MinVersion:         tls.VersionTLS13,
	}

	// ── Hub relay: a real QUIC listener on an ephemeral loopback port ──
	hubAddr := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))
	hub := &Server{
		MOQServer: &moqt.Server{Addr: hubAddr, TLSConfig: serverTLS, QUICConfig: quicCfg},
		MOQDialer: &moqt.Dialer{TLSConfig: dialerTLS, QUICConfig: quicCfg},
		Config:    &Config{NodeID: "hub-1", Role: "hub"},
	}
	go func() { _ = hub.ListenAndServe() }()
	t.Cleanup(func() {
		// Shutdown (not Close) so teardown can't deadlock: while the edge still
		// holds the peer session open, Close() blocks forever; Shutdown honours
		// the timeout and force-closes, which lets the edge unwind.
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = hub.Shutdown(shutCtx)
	})

	// Wait until the hub is accepting QUIC/MOQT sessions before starting the edge,
	// so the edge's first dial succeeds rather than entering the 5s retry backoff.
	require.Eventually(t, func() bool {
		probe := &moqt.Dialer{TLSConfig: dialerTLS, QUICConfig: quicCfg}
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		sess, derr := probe.Dial(ctx, peerURL(hubAddr), moqt.NewTrackMux(0))
		if derr != nil {
			return false
		}
		_ = sess.CloseWithError(0, "probe")
		return true
	}, 5*time.Second, 100*time.Millisecond, "hub never became reachable")

	// ── Edge relay: UpstreamAddr pointed directly at the hub ──
	edge := &Server{
		MOQServer: &moqt.Server{Addr: "127.0.0.1:0", TLSConfig: serverTLS, QUICConfig: quicCfg},
		MOQDialer: &moqt.Dialer{TLSConfig: dialerTLS, QUICConfig: quicCfg},
		Config: &Config{
			NodeID: "edge-1", Role: "edge",
			UpstreamAddr: hubAddr,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go edge.ConnectPeers(ctx)

	// ── Assert: the edge dialed UpstreamAddr and completed the handshake ──
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(metricPeerDialAttempts.WithLabelValues(hubAddr, "ok")) >= 1
	}, 10*time.Second, 200*time.Millisecond, "edge never completed a QUIC handshake to the upstream hub")

	require.GreaterOrEqual(t, testutil.ToFloat64(metricPeersConnected), 1.0,
		"peers_connected should reflect the maintained edge→hub connection")
}

// createTempCert generates an ephemeral self-signed cert and returns the file paths.
func createTempCert(t *testing.T) (string, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	certFile := filepath.Join(t.TempDir(), "server.crt")
	keyFile := filepath.Join(t.TempDir(), "server.key")

	err = os.WriteFile(certFile, certPEM, 0600)
	require.NoError(t, err)

	err = os.WriteFile(keyFile, keyPEM, 0600)
	require.NoError(t, err)

	return certFile, keyFile
}
