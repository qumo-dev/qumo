//go:build integration

package relay

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/devcert"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startPeerRelay starts a relay on a free loopback port with its own peer
// certificate from ca, and returns it and its address.
func startPeerRelay(t *testing.T, ca *devcert.CA, name string) (*Server, string) {
	t.Helper()
	own := issue(t, ca, peerLeaf(name))
	public := issue(t, ca, devcert.Leaf{CommonName: "localhost", DNSNames: []string{"localhost"}, ExtKeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	trust := &PeerTrust{ca: rootsOf(ca), cert: &own, own: own.Certificate[0]}
	quicCfg := &quic.Config{KeepAlivePeriod: time.Second, MaxIdleTimeout: 10 * time.Second}
	addr := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))
	s := &Server{
		MOQServer: &moqt.Server{
			Addr:       addr,
			TLSConfig:  trust.ServerTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{public}, NextProtos: []string{"h3", moqt.NextProtoMOQ}}),
			QUICConfig: quicCfg,
		},
		MOQDialer:       &moqt.Dialer{TLSConfig: trust.DialerTLS(), QUICConfig: quicCfg},
		Config:          &Config{NodeID: name},
		TrackMux:        moqt.NewTrackMux(moqt.NewHopID()),
		Authorize:       admitUnchecked,
		PeerCertificate: trust.OwnCertificate(),
	}
	served := make(chan error, 1)
	go func() { served <- s.ListenAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Logf("%s: shutdown: %v", name, err)
		}
		t.Logf("%s: stopped serving: %v", name, <-served)
	})
	return s, addr
}

// TestPeerResolve_SessionsFollowTheAnswer runs three relays: a dials the
// relays an SRV name answers, which include a itself. It must hold sessions
// to b and c and not to itself, drop c when c leaves the answer, take it
// back when it returns, and keep both through a DNS outage.
func TestPeerResolve_SessionsFollowTheAnswer(t *testing.T) {
	ca := testCA(t)
	a, aAddr := startPeerRelay(t, ca, "relay-a")
	_, bAddr := startPeerRelay(t, ca, "relay-b")
	_, cAddr := startPeerRelay(t, ca, "relay-c")

	r := &fakePeerResolver{}
	srv := func(addr, target string) *net.SRV {
		_, port, err := net.SplitHostPort(addr)
		require.NoError(t, err)
		var p uint16
		_, err = fmt.Sscan(port, &p)
		require.NoError(t, err)
		r.answer(target, "127.0.0.1")
		return &net.SRV{Target: target, Port: p}
	}
	recA, recB, recC := srv(aAddr, "a.relays.test."), srv(bAddr, "b.relays.test."), srv(cAddr, "c.relays.test.")
	const name = "_moqt._udp.relays.test"
	r.answerSRV(name, recA, recB, recC)

	a.resolver = r
	a.Config.Peers = []Peer{{Address: name}}
	a.Config.PeerResolveInterval = 300 * time.Millisecond
	selfDials := metricPeerDialAttempts.WithLabelValues(aAddr, "ok")
	selfBefore := testutil.ToFloat64(selfDials)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		assert.NoError(t, a.ConnectPeers(ctx))
	}()
	t.Cleanup(func() { cancel(); <-done })

	both := []string{bAddr, cAddr}
	if cAddr < bAddr {
		both = []string{cAddr, bAddr}
	}
	require.Eventually(t, func() bool { return assert.ObjectsAreEqual(both, a.ConnectedPeers()) },
		10*time.Second, 50*time.Millisecond, "a peers with b and c")
	t.Logf("peered: %v", a.ConnectedPeers())

	// Several intervals on, a still answers itself, and hasn't dialed it again.
	time.Sleep(time.Second)
	assert.Equal(t, both, a.ConnectedPeers(), "a holds no session to itself")
	assert.Equal(t, 1.0, testutil.ToFloat64(selfDials)-selfBefore, "a dialed itself once")

	r.answerSRV(name, recA, recB)
	require.Eventually(t, func() bool { return assert.ObjectsAreEqual([]string{bAddr}, a.ConnectedPeers()) },
		5*time.Second, 50*time.Millisecond, "c left the answer: a drops it")
	t.Logf("after c left: %v", a.ConnectedPeers())

	r.answerSRV(name, recA, recB, recC)
	require.Eventually(t, func() bool { return assert.ObjectsAreEqual(both, a.ConnectedPeers()) },
		10*time.Second, 50*time.Millisecond, "c is back: a dials it again")
	t.Logf("after c returned: %v", a.ConnectedPeers())

	r.fail(errors.New("i/o timeout"))
	time.Sleep(time.Second)
	assert.Equal(t, both, a.ConnectedPeers(), "a DNS outage drops no peer")
}
