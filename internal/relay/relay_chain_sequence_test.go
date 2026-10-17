//go:build integration

package relay

import (
	"context"
	"io"
	"log/slog"
	"sync"
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

// A publisher reports dropped groups upstream via DropGroups. Through two relays,
// a subscriber receives every drop notification and its track ends promptly
// (AcceptGroup returns io.EOF) when the publisher ends the track.
func TestRelayChain_ForwardsDrops(t *testing.T) {
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
		// Send group 1
		gw, err := tw.OpenGroupAt(ctx, 1)
		if err != nil {
			return
		}
		frame := moqt.NewFrame(8)
		_, _ = frame.Write([]byte("payload1"))
		_ = gw.WriteFrame(frame)
		_ = gw.Close()

		// Drop groups 2..4
		err = tw.DropGroups(moqt.SubscribeDrop{StartGroup: 2, EndGroup: 4, ErrorCode: moqt.SubscribeErrorCodeInternal})
		if err != nil {
			return
		}

		// Send group 5
		gw, err = tw.OpenGroupAt(ctx, 5)
		if err != nil {
			return
		}
		frame = moqt.NewFrame(8)
		_, _ = frame.Write([]byte("payload5"))
		_ = gw.WriteFrame(frame)
		_ = gw.Close()

		// Drop groups 6..8
		err = tw.DropGroups(moqt.SubscribeDrop{StartGroup: 6, EndGroup: 8, ErrorCode: moqt.SubscribeErrorCodeInternal})
		if err != nil {
			return
		}

		// Send group 9
		gw, err = tw.OpenGroupAt(ctx, 9)
		if err != nil {
			return
		}
		frame = moqt.NewFrame(8)
		_, _ = frame.Write([]byte("payload9"))
		_ = gw.WriteFrame(frame)
		_ = gw.Close()
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

	var (
		dropsMu  sync.Mutex
		gotDrops []moqt.SubscribeDrop
	)
	dropCtx, dropCancel := context.WithCancel(ctx)
	defer dropCancel()
	go func() {
		for drop := range tr.Drops(dropCtx) {
			dropsMu.Lock()
			gotDrops = append(gotDrops, drop)
			dropsMu.Unlock()
		}
	}()

	var gotSeqs []moqt.GroupSequence
	for {
		gr, err := tr.AcceptGroup(ctx)
		if err != nil {
			assert.ErrorIs(t, err, io.EOF)
			break
		}
		gotSeqs = append(gotSeqs, gr.GroupSequence())
	}

	assert.ElementsMatch(t, []moqt.GroupSequence{1, 5, 9}, gotSeqs)

	assert.Eventually(t, func() bool {
		dropsMu.Lock()
		defer dropsMu.Unlock()
		return len(gotDrops) == 2
	}, 3*time.Second, 10*time.Millisecond)

	dropsMu.Lock()
	defer dropsMu.Unlock()
	assert.Equal(t, []moqt.SubscribeDrop{
		{StartGroup: 2, EndGroup: 4, ErrorCode: moqt.SubscribeErrorCodeInternal},
		{StartGroup: 6, EndGroup: 8, ErrorCode: moqt.SubscribeErrorCodeInternal},
	}, gotDrops)
}

// When a subscriber falls behind, the relay skips groups and reports those
// dropped sequences via DropGroups. The subscriber receives the drops and
// settles cleanly at the end of the track.
func TestRelayChain_ReportsSubscriberSkips(t *testing.T) {
	t.Setenv("RELAY_RING", "2")
	cert, pool := chainCert(t)
	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	origin := spinRelay(t, "origin", chainFreeAddr(t), cert, pool, quicCfg)
	leaf := spinRelay(t, "leaf", chainFreeAddr(t), cert, pool, quicCfg)
	leaf.Config.Peers = []Peer{{Address: origin.MOQServer.Addr}}
	go leaf.ConnectPeers(ctx) //nolint:errcheck // ends with ctx

	pubDone := make(chan struct{})
	pubGate := make(chan struct{})
	pubMux := moqt.NewTrackMux(moqt.NewHopID())
	pubMux.PublishFunc(ctx, chainBroadcastPath, func(tw *moqt.TrackWriter) {
		// Send group 1
		gw, err := tw.OpenGroupAt(ctx, 1)
		if err != nil {
			return
		}
		frame := moqt.NewFrame(8)
		_, _ = frame.Write([]byte("group1"))
		_ = gw.WriteFrame(frame)
		_ = gw.Close()

		// Wait until subscriber has received group 1
		select {
		case <-pubGate:
		case <-ctx.Done():
			return
		}

		// Burst groups 2 through 10 to overflow the cache (size 2) and force a skip
		for i := uint64(2); i <= 10; i++ {
			gw, err := tw.OpenGroupAt(ctx, moqt.GroupSequence(i))
			if err != nil {
				slog.Info("publisher OpenGroupAt failed", "seq", i, "err", err)
				return
			}
			f := moqt.NewFrame(8)
			_, _ = f.Write([]byte("payload"))
			_ = gw.WriteFrame(f)
			_ = gw.Close()
			time.Sleep(10 * time.Millisecond)
		}
		close(pubDone)
	})
	pubSess, err := (&moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: &quic.Config{EnableDatagrams: true}}).Dial(ctx, "moqt://"+origin.MOQServer.Addr, pubMux)
	require.NoError(t, err)
	defer pubSess.CloseWithError(moqt.NoError, "done") //nolint:errcheck // test teardown
	waitForHandler(t, leaf, chainBroadcastPath)

	subQUICCfg := &quic.Config{
		EnableDatagrams:       true,
		MaxIncomingUniStreams: 2,
	}
	sess, err := (&moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: subQUICCfg}).Dial(ctx, "moqt://"+leaf.MOQServer.Addr, moqt.NewTrackMux(0))
	require.NoError(t, err)
	defer sess.CloseWithError(moqt.NoError, "done") //nolint:errcheck // test teardown
	tr, err := sess.Subscribe(ctx, chainBroadcastPath, chainTrackName, nil)
	require.NoError(t, err)
	defer tr.Close() //nolint:errcheck // test teardown

	var (
		dropsMu  sync.Mutex
		gotDrops []moqt.SubscribeDrop
	)
	dropCtx, dropCancel := context.WithCancel(ctx)
	defer dropCancel()
	go func() {
		for drop := range tr.Drops(dropCtx) {
			dropsMu.Lock()
			gotDrops = append(gotDrops, drop)
			dropsMu.Unlock()
		}
	}()

	// Read group 1
	gr, err := tr.AcceptGroup(ctx)
	require.NoError(t, err)
	assert.Equal(t, moqt.GroupSequence(1), gr.GroupSequence())

	// Signal publisher to send groups 2 through 10
	close(pubGate)

	// Wait until publisher has pushed all groups to origin, ensuring the
	// subscriber (holding max 2 uni-streams) falls behind the ring of size 2.
	select {
	case <-pubDone:
	case <-ctx.Done():
		t.Fatal("publisher timed out")
	}

	// Now read the rest of the groups until EOF
	var gotSeqs []moqt.GroupSequence
	gotSeqs = append(gotSeqs, gr.GroupSequence())
	for {
		gr, err := tr.AcceptGroup(ctx)
		if err != nil {
			assert.ErrorIs(t, err, io.EOF)
			break
		}
		gotSeqs = append(gotSeqs, gr.GroupSequence())
	}

	t.Logf("gotSeqs: %v, gotDrops: %v", gotSeqs, gotDrops)

	// Subscriber should have received the initial group, skipped some groups,
	// and received drop notifications for all missing sequences.
	assert.Contains(t, gotSeqs, moqt.GroupSequence(1))
	assert.Less(t, len(gotSeqs), 10, "some groups should have been dropped under backpressure")

	dropsMu.Lock()
	defer dropsMu.Unlock()
	assert.NotEmpty(t, gotDrops, "subscriber should have received drop notifications for skipped groups")

	accounted := make(map[moqt.GroupSequence]bool)
	for _, seq := range gotSeqs {
		accounted[seq] = true
	}
	for _, drop := range gotDrops {
		for s := drop.StartGroup; s <= drop.EndGroup; s++ {
			accounted[s] = true
		}
	}
	for i := uint64(1); i <= 10; i++ {
		assert.True(t, accounted[moqt.GroupSequence(i)], "group sequence %d should be accounted for", i)
	}
}
