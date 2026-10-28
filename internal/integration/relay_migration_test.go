//go:build integration

// Black-box tests of QUIC connection migration (RFC 9000 §9) against a real
// relay: a subscriber whose network changes mid-session, as a phone moving
// from Wi-Fi to cellular does, keeps receiving its subscription over the same
// QUIC connection and MOQ session, without a new handshake.
//
// The relay does nothing for this itself: quic-go validates the new path and
// moves the connection to it. These tests pin that behaviour so a relay or
// quic-go change that breaks it fails here.
package integration

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const migrationPath = moqt.BroadcastPath("/mobile/live")

func TestRelay_ConnectionMigration(t *testing.T) {
	addr, srv := startAuthRelay(t, authOff, nil)
	publishOver(t, srv, "https://"+addr+"/", migrationPath)
	// Opened before the session so that, cleanups running last-in first-out,
	// the session closes while the cellular path still carries its close.
	cellular := &fakeCountingConn{PacketConn: listenUDP(t)}
	cellularTr := &quic.Transport{Conn: cellular, ConnectionIDGenerator: zeroConnIDs{}}
	t.Cleanup(func() { _ = cellularTr.Close() }) // not actionable: cleanup
	sub := dialMigratable(t, addr)
	tr := subscribeMigration(t, sub.sess)
	last := nextGroup(t, tr, 5*time.Second)

	// The phone joins cellular: probe a path from a new socket, then move the
	// connection onto it.
	path, err := sub.sess.AddPath(cellularTr)
	require.NoError(t, err)
	probeCtx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	require.NoError(t, path.Probe(probeCtx), "the relay should answer the PATH_CHALLENGE")
	require.NoError(t, path.Switch())
	// The relay follows once a non-probing packet arrives on the new path:
	// the subscriber's next acknowledgement of a group, 50 ms away at most.
	// Wi-Fi stays up until the relay has moved: quic-go's client answers the
	// relay's PATH_CHALLENGE on the active path, not the probed one
	// (quic-go#5408; fixed by quic-go#5980, not in a release as of v0.64.1),
	// so cutting it first can stall the relay's validation of the new path.
	last = readUntilCellular(t, tr, cellular, last)
	// Wi-Fi is gone: nothing more is sent or received over the old socket.
	sub.wifi.down.Store(true)

	readUntilCellular(t, tr, cellular, last)

	assert.NoError(t, sub.sess.Context().Err(), "the session, and the QUIC connection under it, should survive the move")
}

// Without a switch, losing Wi-Fi stops the subscription: this proves the
// dropped socket really cuts the old path, so the test above passes only
// because the connection moved.
func TestRelay_ConnectionMigration_LinkLostWithoutSwitch(t *testing.T) {
	addr, srv := startAuthRelay(t, authOff, nil)
	publishOver(t, srv, "https://"+addr+"/", migrationPath)
	sub := dialMigratable(t, addr)
	tr := subscribeMigration(t, sub.sess)
	nextGroup(t, tr, 5*time.Second)

	sub.wifi.down.Store(true)
	// Bring the link back before the session closes, so its close reaches
	// the relay instead of leaving it to time the session out.
	t.Cleanup(func() { sub.wifi.down.Store(false) })

	// Groups already queued in the client may still be delivered; after them
	// delivery must go quiet, while the publisher keeps sending every 50 ms.
	assert.True(t, deliveryStops(t, tr, time.Second, 3*time.Second), "groups kept arriving over a dead path")
}

// migratableSub is a native-QUIC subscriber session dialed over a socket
// the test can cut.
type migratableSub struct {
	sess *moqt.Session
	wifi *fakeDroppableConn
}

// dialMigratable dials the relay at addr over native QUIC through
// Dialer.Transport, from a socket the test can cut.
func dialMigratable(tb testing.TB, addr string) migratableSub {
	tb.Helper()
	sub := migratableSub{wifi: &fakeDroppableConn{PacketConn: listenUDP(tb)}}
	wifi := &quic.Transport{Conn: sub.wifi, ConnectionIDGenerator: zeroConnIDs{}}
	tb.Cleanup(func() { _ = wifi.Close() }) // not actionable: cleanup
	dialer := &moqt.Dialer{
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // test-only self-signed cert
			MinVersion:         tls.VersionTLS13,
		},
		// The config every other integration-test client dials with.
		QUICConfig: &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second},
		Transport:  wifi,
	}
	ctx, cancel := context.WithTimeout(tb.Context(), 3*time.Second)
	defer cancel()
	sess, err := dialer.Dial(ctx, nativeURL(addr)+"/", moqt.NewTrackMux(0))
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = sess.CloseWithError(moqt.NoError, "test done") }) // not actionable: cleanup
	sub.sess = sess
	return sub
}

func subscribeMigration(tb testing.TB, sess *moqt.Session) *moqt.TrackReader {
	tb.Helper()
	ctx, cancel := context.WithTimeout(tb.Context(), 5*time.Second)
	defer cancel()
	tr, err := sess.Subscribe(ctx, migrationPath, testTrack, nil)
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = tr.Close() }) // not actionable: cleanup
	return tr
}

// nextGroup returns the sequence of the next group on tr.
func nextGroup(tb testing.TB, tr *moqt.TrackReader, timeout time.Duration) moqt.GroupSequence {
	tb.Helper()
	ctx, cancel := context.WithTimeout(tb.Context(), timeout)
	defer cancel()
	gr, err := tr.AcceptGroup(ctx)
	require.NoError(tb, err)
	return drain(gr)
}

// nextGroupAfter returns the sequence of the first group on tr later than
// last, skipping any older one still in flight.
func nextGroupAfter(tb testing.TB, tr *moqt.TrackReader, last moqt.GroupSequence, timeout time.Duration) moqt.GroupSequence {
	tb.Helper()
	ctx, cancel := context.WithTimeout(tb.Context(), timeout)
	defer cancel()
	for {
		gr, err := tr.AcceptGroup(ctx)
		require.NoError(tb, err, "no group after %d", last)
		if seq := drain(gr); seq > last {
			return seq
		}
	}
}

// readUntilCellular reads groups later than last from tr until about a second
// of live groups (one datagram each, every 50 ms) has arrived on the cellular
// socket, and returns the last sequence read. The relay starts a subscriber
// at its oldest held group, so groups already queued in the client could
// keep arriving without a move; datagrams on the new socket cannot. Groups
// are read meanwhile, since unread ones hold the stream credit the relay
// needs to send more.
func readUntilCellular(tb testing.TB, tr *moqt.TrackReader, cellular *fakeCountingConn, last moqt.GroupSequence) moqt.GroupSequence {
	tb.Helper()
	before := cellular.reads.Load()
	deadline := time.Now().Add(5 * time.Second)
	for cellular.reads.Load()-before < 20 {
		require.True(tb, time.Now().Before(deadline), "the relay should send over the cellular path")
		last = nextGroupAfter(tb, tr, last, 5*time.Second)
	}
	return last
}

// deliveryStops reports whether, within limit, tr goes quiet for at least
// quiet: no group accepted for that long.
func deliveryStops(tb testing.TB, tr *moqt.TrackReader, quiet, limit time.Duration) bool {
	tb.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(tb.Context(), quiet)
		gr, err := tr.AcceptGroup(ctx)
		cancel()
		if err != nil {
			return true
		}
		drain(gr)
	}
	return false
}

// drain reads gr to its end, so its stream closes and quic-go returns the
// stream credit, and returns its sequence.
func drain(gr *moqt.GroupReader) moqt.GroupSequence {
	_ = gr.SetReadDeadline(time.Now().Add(time.Second)) // not actionable: a stuck group only ends the read early
	for range gr.Frames(moqt.NewFrame(64)) {
	}
	return gr.GroupSequence()
}

func listenUDP(tb testing.TB) net.PacketConn {
	tb.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = conn.Close() }) // not actionable: cleanup
	return conn
}

// zeroConnIDs generates zero-length connection IDs, which every transport
// of a connection that moves should use (see moqt.Session.AddPath): a
// quic-go relay follows a client with longer ones only twice (quic-go#5965).
type zeroConnIDs struct{}

var _ quic.ConnectionIDGenerator = zeroConnIDs{}

func (zeroConnIDs) GenerateConnectionID() (quic.ConnectionID, error) {
	return quic.ConnectionID{}, nil
}

func (zeroConnIDs) ConnectionIDLen() int { return 0 }
