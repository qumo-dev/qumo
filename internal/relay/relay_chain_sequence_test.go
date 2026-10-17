//go:build integration

package relay

import (
	"context"
	"crypto/x509"
	"io"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A publisher numbers its groups as it likes. Through two relays, a subscriber
// receives every group under the sequence its publisher gave it, whether the
// sequences start past 1, skip some, or do both.
func TestRelayChain_KeepsPublisherSequences(t *testing.T) {
	tests := map[string][]moqt.GroupSequence{
		"starting past one": {7, 8, 9},
		"with gaps":         {1, 5, 9},
		"both":              {40, 43, 44, 100},
	}
	for name, seqs := range tests {
		t.Run(name, func(t *testing.T) {
			cert, pool := chainCert(t)
			quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			origin := spinRelay(t, "origin", chainFreeAddr(t), cert, pool, quicCfg)
			leaf := spinRelay(t, "leaf", chainFreeAddr(t), cert, pool, quicCfg)
			leaf.Config.Peers = []Peer{{Address: origin.MOQServer.Addr}}
			go leaf.ConnectPeers(ctx) //nolint:errcheck // ends with ctx

			pubMux := moqt.NewTrackMux(moqt.NewHopID())
			pubMux.PublishFunc(ctx, chainBroadcastPath, func(tw *moqt.TrackWriter) {
				for _, seq := range seqs {
					gw, err := tw.OpenGroupAt(ctx, seq)
					if err != nil {
						return
					}
					frame := moqt.NewFrame(8)
					_, _ = frame.Write([]byte("payload"))
					_ = gw.WriteFrame(frame)
					_ = gw.Close()
				}
				// The track stays open: the subscriber leaves first.
				<-tw.Context().Done()
			})
			pubSess, err := (&moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: &quic.Config{EnableDatagrams: true}}).Dial(ctx, "moqt://"+origin.MOQServer.Addr, pubMux)
			require.NoError(t, err)
			defer pubSess.CloseWithError(moqt.NoError, "done") //nolint:errcheck // test teardown
			waitForHandler(t, leaf, chainBroadcastPath)

			sess, err := (&moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: &quic.Config{EnableDatagrams: true}}).Dial(ctx, "moqt://"+leaf.MOQServer.Addr, moqt.NewTrackMux(0))
			require.NoError(t, err)
			defer sess.CloseWithError(moqt.NoError, "done") //nolint:errcheck // test teardown
			tr, err := sess.Subscribe(ctx, chainBroadcastPath, chainTrackName, nil)
			require.NoError(t, err)
			defer tr.Close() //nolint:errcheck // test teardown

			var got []moqt.GroupSequence
			for range seqs {
				gr, err := tr.AcceptGroup(ctx)
				require.NoError(t, err, "received %v of %v", got, seqs)
				got = append(got, gr.GroupSequence())
			}

			assert.ElementsMatch(t, seqs, got)
		})
	}
}

// startRelayChain starts an origin relay and a leaf relay peered to it, and
// returns them with the pool that trusts their certificate.
func startRelayChain(tb testing.TB, ctx context.Context) (*Server, *Server, *x509.CertPool) {
	tb.Helper()
	cert, pool := chainCert(tb)
	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}
	origin := spinRelay(tb, "origin", chainFreeAddr(tb), cert, pool, quicCfg)
	leaf := spinRelay(tb, "leaf", chainFreeAddr(tb), cert, pool, quicCfg)
	leaf.Config.Peers = []Peer{{Address: origin.MOQServer.Addr}}
	go leaf.ConnectPeers(ctx) //nolint:errcheck // ends with ctx
	return origin, leaf, pool
}

// publishToChain dials origin as a publisher whose track runs publish, and
// waits until leaf has the announcement.
func publishToChain(tb testing.TB, ctx context.Context, origin, leaf *Server, pool *x509.CertPool, publish func(tw *moqt.TrackWriter)) {
	tb.Helper()
	pubMux := moqt.NewTrackMux(moqt.NewHopID())
	pubMux.PublishFunc(ctx, chainBroadcastPath, publish)
	pubSess, err := (&moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: &quic.Config{EnableDatagrams: true}}).Dial(ctx, "moqt://"+origin.MOQServer.Addr, pubMux)
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = pubSess.CloseWithError(moqt.NoError, "done") }) // not actionable: test teardown
	waitForHandler(tb, leaf, chainBroadcastPath)
}

// subscribeToChain dials leaf with quicCfg and subscribes to the test track.
func subscribeToChain(tb testing.TB, ctx context.Context, leaf *Server, pool *x509.CertPool, quicCfg *quic.Config) *moqt.TrackReader {
	tb.Helper()
	sess, err := (&moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: quicCfg}).Dial(ctx, "moqt://"+leaf.MOQServer.Addr, moqt.NewTrackMux(0))
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = sess.CloseWithError(moqt.NoError, "done") }) // not actionable: test teardown
	tr, err := sess.Subscribe(ctx, chainBroadcastPath, chainTrackName, nil)
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = tr.Close() }) // not actionable: test teardown
	return tr
}

// writeGroup writes one single-frame group with sequence seq.
func writeGroup(ctx context.Context, tw *moqt.TrackWriter, seq moqt.GroupSequence) error {
	gw, err := tw.OpenGroupAt(ctx, seq)
	if err != nil {
		return err
	}
	defer gw.Close() //nolint:errcheck // the frame is already written
	frame := moqt.NewFrame(8)
	_, _ = frame.Write([]byte("payload")) // not actionable: writes to memory
	return gw.WriteFrame(frame)
}

// readGroup reads every frame of gr so its stream finishes and the
// subscriber can accept another.
func readGroup(gr *moqt.GroupReader) {
	for range gr.Frames(nil) {
	}
}

// readToEOF accepts and reads groups until the track ends and returns their
// sequences in arrival order.
func readToEOF(tb testing.TB, ctx context.Context, tr *moqt.TrackReader) []moqt.GroupSequence {
	tb.Helper()
	var seqs []moqt.GroupSequence
	for {
		gr, err := tr.AcceptGroup(ctx)
		if err != nil {
			require.ErrorIs(tb, err, io.EOF, "received %v before the error", seqs)
			return seqs
		}
		seqs = append(seqs, gr.GroupSequence())
		readGroup(gr)
	}
}

// waitGate blocks until gate is closed and reports false if ctx ends first.
func waitGate(ctx context.Context, gate <-chan struct{}) bool {
	select {
	case <-gate:
		return true
	case <-ctx.Done():
		return false
	}
}

// queuedDrops returns the drops tr has received and not yet returned. It
// does not wait: the context is already cancelled, and Drops yields queued
// drops before it checks the context. SUBSCRIBE_DROP and SUBSCRIBE_END travel
// in order on the subscribe stream, so once AcceptGroup has returned io.EOF
// every drop is queued.
func queuedDrops(tr *moqt.TrackReader) []moqt.SubscribeDrop {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var drops []moqt.SubscribeDrop
	for drop := range tr.Drops(ctx) {
		drops = append(drops, drop)
	}
	return drops
}

// A publisher reports dropped groups upstream via DropGroups. Through two
// relays, a subscriber receives each drop once, with the publisher's range and
// error code, and its track ends (io.EOF) without waiting out the grace period.
//
// Group streams can reach a relay out of order, and gomoqt starts a
// subscription at the first group opened; drops below that start are not
// forwarded because the subscriber does not wait for them. The publisher
// therefore holds everything after group 1 until the subscriber has it, so
// the start is group 1 at every hop.
func TestRelayChain_ForwardsDrops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	origin, leaf, pool := startRelayChain(t, ctx)

	gotFirst := make(chan struct{})
	publishToChain(t, ctx, origin, leaf, pool, func(tw *moqt.TrackWriter) {
		if writeGroup(ctx, tw, 1) != nil || !waitGate(ctx, gotFirst) {
			return
		}
		if tw.DropGroups(dropRange(2, 4, moqt.SubscribeErrorCodeInternal)) != nil {
			return
		}
		if writeGroup(ctx, tw, 5) != nil {
			return
		}
		if tw.DropGroups(dropRange(6, 8, moqt.SubscribeErrorCodeTimeout)) != nil {
			return
		}
		_ = writeGroup(ctx, tw, 9) // not actionable: the subscriber checks what arrived
	})
	tr := subscribeToChain(t, ctx, leaf, pool, &quic.Config{EnableDatagrams: true})
	first, err := tr.AcceptGroup(ctx)
	require.NoError(t, err)
	require.Equal(t, moqt.GroupSequence(1), first.GroupSequence())
	readGroup(first)
	close(gotFirst)

	gotSeqs := readToEOF(t, ctx, tr)

	assert.ElementsMatch(t, []moqt.GroupSequence{5, 9}, gotSeqs)
	assert.Equal(t, []moqt.SubscribeDrop{
		dropRange(2, 4, moqt.SubscribeErrorCodeInternal),
		dropRange(6, 8, moqt.SubscribeErrorCodeTimeout),
	}, queuedDrops(tr))
}

// A subscriber that joins after the relay forwarded drops starts at the
// newest group and is not sent the earlier drops, which are below its start.
func TestRelayChain_LateSubscriberGetsNoEarlierDrops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	origin, leaf, pool := startRelayChain(t, ctx)

	gotFirst := make(chan struct{})
	resume := make(chan struct{})
	publishToChain(t, ctx, origin, leaf, pool, func(tw *moqt.TrackWriter) {
		if writeGroup(ctx, tw, 1) != nil || !waitGate(ctx, gotFirst) {
			return
		}
		if tw.DropGroups(dropRange(2, 4, moqt.SubscribeErrorCodeInternal)) != nil {
			return
		}
		if writeGroup(ctx, tw, 5) != nil || !waitGate(ctx, resume) {
			return
		}
		_ = writeGroup(ctx, tw, 6) // not actionable: the subscribers check what arrived
	})

	// The first subscriber makes the leaf hold the track and record the drop.
	// Group 1 reaches it before anything else, so its start is group 1.
	first := subscribeToChain(t, ctx, leaf, pool, &quic.Config{EnableDatagrams: true})
	for _, want := range []moqt.GroupSequence{1, 5} {
		gr, err := first.AcceptGroup(ctx)
		require.NoError(t, err)
		require.Equal(t, want, gr.GroupSequence())
		readGroup(gr)
		if want == 1 {
			close(gotFirst)
		}
	}

	// The second subscriber joins at group 5, after drop 2..4.
	late := subscribeToChain(t, ctx, leaf, pool, &quic.Config{EnableDatagrams: true})
	gr, err := late.AcceptGroup(ctx)
	require.NoError(t, err)
	require.Equal(t, moqt.GroupSequence(5), gr.GroupSequence())
	readGroup(gr)
	close(resume)

	assert.Equal(t, []moqt.GroupSequence{6}, readToEOF(t, ctx, late))
	assert.Empty(t, queuedDrops(late))
	assert.Equal(t, []moqt.GroupSequence{6}, readToEOF(t, ctx, first))
	assert.Equal(t, []moqt.SubscribeDrop{dropRange(2, 4, moqt.SubscribeErrorCodeInternal)}, queuedDrops(first))
}

// A subscriber that cannot accept new group streams falls behind: the relay
// times out opening groups and skips ahead in its ring, and reports every
// group it does not deliver. Each of the publisher's groups then reaches the
// subscriber exactly once, either delivered or inside one drop, and the track
// ends without waiting out the grace period.
func TestRelayChain_ReportsSubscriberSkips(t *testing.T) {
	const lastGroup = 10
	t.Setenv("RELAY_RING", "2")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	origin, leaf, pool := startRelayChain(t, ctx)

	burst := make(chan struct{})
	publishToChain(t, ctx, origin, leaf, pool, func(tw *moqt.TrackWriter) {
		if writeGroup(ctx, tw, 1) != nil || !waitGate(ctx, burst) {
			return
		}
		for seq := moqt.GroupSequence(2); seq <= lastGroup; seq++ {
			if writeGroup(ctx, tw, seq) != nil {
				return
			}
		}
	})

	// Two incoming streams and no reading: the relay can open at most groups
	// 1 and 2, and every later open times out.
	tr := subscribeToChain(t, ctx, leaf, pool, &quic.Config{EnableDatagrams: true, MaxIncomingUniStreams: 2})
	first, err := tr.AcceptGroup(ctx)
	require.NoError(t, err)
	require.Equal(t, moqt.GroupSequence(1), first.GroupSequence())
	close(burst)

	// Hold group 1 open until the relay has reported a drop, so the subscriber
	// has certainly fallen behind before it starts reading.
	var gotDrops []moqt.SubscribeDrop
	require.Eventually(t, func() bool {
		gotDrops = append(gotDrops, queuedDrops(tr)...)
		return len(gotDrops) > 0
	}, 10*time.Second, 10*time.Millisecond, "the relay never reported a drop")
	readGroup(first)

	gotSeqs := append([]moqt.GroupSequence{1}, readToEOF(t, ctx, tr)...)
	gotDrops = append(gotDrops, queuedDrops(tr)...)

	seen := make(map[moqt.GroupSequence]int)
	for _, seq := range gotSeqs {
		seen[seq]++
	}
	for _, drop := range gotDrops {
		assert.Contains(t, []moqt.SubscribeErrorCode{moqt.SubscribeErrorCodeInternal, moqt.SubscribeErrorCodeTimeout}, drop.ErrorCode, "drop %+v", drop)
		for seq := drop.StartGroup; seq <= drop.EndGroup; seq++ {
			seen[seq]++
		}
	}
	for seq := moqt.GroupSequence(1); seq <= lastGroup; seq++ {
		assert.Equal(t, 1, seen[seq], "group %d: delivered %v, dropped %v", seq, gotSeqs, gotDrops)
	}
	assert.Len(t, seen, lastGroup, "no group outside 1..%d is delivered or dropped", lastGroup)
}
