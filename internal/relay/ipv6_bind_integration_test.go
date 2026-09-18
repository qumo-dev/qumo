//go:build integration

// Package relay integration test for the dual-stack bind (#234 / #238): the
// relay's default ":port" RELAY_ADDR must bind both IPv4 and IPv6 so a client
// — notably a browser whose `localhost` resolves to ::1 (Windows) — can still
// connect. Run with `go test -tags=integration ./internal/relay/...`.
package relay

import (
	"context"
	"crypto/tls"
	"fmt"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/require"
)

// TestServer_DualStackBind_ReachableOverIPv6 stands up a relay bound to ":port"
// (the dual-stack style of the default RELAY_ADDR) and asserts a client can
// complete a QUIC/MOQT handshake over IPv6 loopback ([::1]) as well as IPv4
// (127.0.0.1). Guards against the #234 regression: an IPv4-only bind made
// https://localhost:<port> reset on hosts where localhost → ::1.
func TestServer_DualStackBind_ReachableOverIPv6(t *testing.T) {
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

	// ":port" — the dual-stack bind style the default RELAY_ADDR now uses.
	port := freeUDPPort(t)
	addr := fmt.Sprintf(":%d", port)
	srv := &Server{
		MOQServer: &moqt.Server{Addr: addr, TLSConfig: serverTLS, QUICConfig: quicCfg},
		MOQDialer: &moqt.Dialer{TLSConfig: dialerTLS, QUICConfig: quicCfg},
		Config: &Config{
			NodeID: "v6-relay", Role: "hub",
		},
	}
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	})

	// Targets a dual-stack bind must reach: IPv6 loopback first (the regression),
	// then IPv4 loopback. Dial each, complete the QUIC/MOQT handshake, close.
	targets := []string{
		fmt.Sprintf("[::1]:%d", port), // IPv6 loopback — the localhost=::1 case
		fmt.Sprintf("127.0.0.1:%d", port),
	}
	require.Eventually(t, func() bool {
		for _, target := range targets {
			probe := &moqt.Dialer{TLSConfig: dialerTLS, QUICConfig: quicCfg}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			sess, derr := probe.Dial(ctx, peerURL(target), moqt.NewTrackMux(0))
			cancel()
			if derr != nil {
				return false
			}
			_ = sess.CloseWithError(0, "probe")
		}
		return true
	}, 5*time.Second, 100*time.Millisecond,
		"relay bound to %q was not reachable over both IPv6 ([::1]) and IPv4 (127.0.0.1) loopback", addr)
}
