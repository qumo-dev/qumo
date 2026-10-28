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
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const migrationPath = moqt.BroadcastPath("/mobile/live")

// A move made while Wi-Fi still works, as when a phone joins cellular before
// it leaves Wi-Fi.
func TestRelay_ConnectionMigration(t *testing.T) {
	addr, srv := startAuthRelay(t, authOff, nil)
	publishOver(t, srv, "https://"+addr+"/", migrationPath)
	// Opened before the session so that, cleanups running last-in first-out,
	// the session closes while the cellular path still carries its close.
	cellular := newCellular(t)
	sub := dialMigratable(t, addr)
	tr := subscribeMigration(t, sub.sess)
	last := nextGroupAfter(t, tr, 0, 5*time.Second)

	moveTo(t, sub.sess, cellular.tr)
	// Wi-Fi stays up until the relay has moved, so the relay's validation of
	// the new path does not need its retries (quic-go#5408: the client answers
	// a PATH_CHALLENGE on its active path; fixed by quic-go#5980, not in a
	// release as of v0.64.1).
	last = readUntilMoved(t, tr, sub.wifi, cellular.conn, last)
	// Wi-Fi is gone: nothing more is sent or received over the old socket.
	sub.wifi.down.Store(true)

	readUntilMoved(t, tr, sub.wifi, cellular.conn, last)

	assert.NoError(t, sub.sess.Context().Err(), "the session, and the QUIC connection under it, should survive the move")
}

// A move made once Wi-Fi is already gone, as when a phone loses Wi-Fi
// first: the relay validates the new path only once the client answers its
// challenge there, after the switch, which its retries wait for.
func TestRelay_ConnectionMigration_FromDeadNetwork(t *testing.T) {
	addr, srv := startAuthRelay(t, authOff, nil)
	publishOver(t, srv, "https://"+addr+"/", migrationPath)
	cellular := newCellular(t)
	sub := dialMigratable(t, addr)
	tr := subscribeMigration(t, sub.sess)
	last := nextGroupAfter(t, tr, 0, 5*time.Second)

	sub.wifi.down.Store(true)
	moveTo(t, sub.sess, cellular.tr)

	readUntilMoved(t, tr, sub.wifi, cellular.conn, last)

	assert.NoError(t, sub.sess.Context().Err(), "the session, and the QUIC connection under it, should survive the move")
}

// Without a switch, losing Wi-Fi stops the subscription: this proves the
// dropped socket really cuts the old path, so the tests above pass only
// because the connection moved.
func TestRelay_ConnectionMigration_LinkLostWithoutSwitch(t *testing.T) {
	addr, srv := startAuthRelay(t, authOff, nil)
	publishOver(t, srv, "https://"+addr+"/", migrationPath)
	sub := dialMigratable(t, addr)
	tr := subscribeMigration(t, sub.sess)
	nextGroupAfter(t, tr, 0, 5*time.Second)

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
	dialer := &moqt.Dialer{TLSConfig: testClientTLS(), QUICConfig: testClientQUIC(), Transport: wifi}
	ctx, cancel := context.WithTimeout(tb.Context(), 3*time.Second)
	defer cancel()
	sess, err := dialer.Dial(ctx, nativeURL(addr)+"/", moqt.NewTrackMux(0))
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = sess.CloseWithError(moqt.NoError, "test done") }) // not actionable: cleanup
	sub.sess = sess
	return sub
}

// cellularNet is a second network: a transport over a socket that counts
// what it reads.
type cellularNet struct {
	tr   *quic.Transport
	conn *fakeCountingConn
}

func newCellular(tb testing.TB) cellularNet {
	tb.Helper()
	conn := &fakeCountingConn{PacketConn: listenUDP(tb)}
	tr := &quic.Transport{Conn: conn, ConnectionIDGenerator: zeroConnIDs{}}
	tb.Cleanup(func() { _ = tr.Close() }) // not actionable: cleanup
	return cellularNet{tr: tr, conn: conn}
}

// moveTo probes a path over t and moves sess onto it. The relay follows once
// a non-probing packet arrives on the new path: the subscriber's next
// acknowledgement of a group, 50 ms away at most.
func moveTo(tb testing.TB, sess *moqt.Session, t *quic.Transport) {
	tb.Helper()
	path, err := sess.AddPath(t)
	require.NoError(tb, err)
	ctx, cancel := context.WithTimeout(tb.Context(), 3*time.Second)
	defer cancel()
	require.NoError(tb, path.Probe(ctx), "the relay should answer the PATH_CHALLENGE")
	require.NoError(tb, path.Switch())
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

// nextGroupAfter returns the sequence of the first group on tr, later than
// last, that arrives whole within timeout. It skips older groups still in
// flight and ones cut off on a dead path.
func nextGroupAfter(tb testing.TB, tr *moqt.TrackReader, last moqt.GroupSequence, timeout time.Duration) moqt.GroupSequence {
	tb.Helper()
	ctx, cancel := context.WithTimeout(tb.Context(), timeout)
	defer cancel()
	for {
		gr, err := tr.AcceptGroup(ctx)
		require.NoError(tb, err, "no group after %d", last)
		if seq, err := drain(gr); err == nil && seq > last {
			return seq
		}
	}
}

// readUntilMoved reads groups later than last from tr until ten in a row,
// half a second of the publisher's, have arrived while wifi read nothing and
// cellular read some: they came over cellular, so the relay has moved. It
// returns the last sequence read. Groups are read meanwhile, since unread
// ones hold the stream credit the relay needs to send more.
func readUntilMoved(tb testing.TB, tr *moqt.TrackReader, wifi *fakeDroppableConn, cellular *fakeCountingConn, last moqt.GroupSequence) moqt.GroupSequence {
	tb.Helper()
	const groups = 10
	deadline := time.Now().Add(5 * time.Second)
	wifiReads, cellularReads, streak := wifi.reads.Load(), cellular.reads.Load(), 0
	for streak < groups {
		remaining := time.Until(deadline)
		require.Positive(tb, remaining, "the relay should send over the cellular path")
		last = nextGroupAfter(tb, tr, last, remaining)
		if n := wifi.reads.Load(); n != wifiReads {
			wifiReads, cellularReads, streak = n, cellular.reads.Load(), 0
			continue
		}
		streak++
	}
	require.Greater(tb, cellular.reads.Load(), cellularReads, "groups arrived with nothing read on either socket")
	return last
}

// deliveryStops reports whether, within limit, tr goes quiet for at least
// quiet: no group accepted for that long. Any other end of the subscription
// fails the test, since it would stop delivery for another reason.
func deliveryStops(tb testing.TB, tr *moqt.TrackReader, quiet, limit time.Duration) bool {
	tb.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(tb.Context(), quiet)
		gr, err := tr.AcceptGroup(ctx)
		timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
		cancel()
		if err != nil {
			require.True(tb, timedOut, "the subscription ended: %v", err)
			return true
		}
		_, _ = drain(gr) // not actionable: a group cut off on the dead path still counts as arriving
	}
	return false
}

// drain reads gr to its end, so its stream closes and quic-go returns the
// stream credit. It returns gr's sequence, and an error if its frames did
// not all arrive within a second; the stream is then cancelled.
func drain(gr *moqt.GroupReader) (moqt.GroupSequence, error) {
	_ = gr.SetReadDeadline(time.Now().Add(time.Second)) // not actionable: without a deadline the read only waits longer
	fr := moqt.NewFrame(64)
	for {
		err := gr.ReadFrame(fr)
		if errors.Is(err, io.EOF) {
			return gr.GroupSequence(), nil
		}
		if err != nil {
			gr.CancelRead(moqt.InternalGroupErrorCode)
			return gr.GroupSequence(), err
		}
	}
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
