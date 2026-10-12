//go:build integration

package relay

import (
	"context"
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
