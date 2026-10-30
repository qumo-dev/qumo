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

// The HLS opt-in, through relays: one session publishes /acme/cam, another
// announces /.hls/acme/cam and answers every subscription with a redirect to
// /acme/cam. A subscriber to /.hls/acme/cam, through one relay or two, gets
// the media of /acme/cam: each relay passes the redirect on instead of
// following it on the opt-in's session, which does not publish /acme/cam.
func TestRelay_PassesRedirectOn(t *testing.T) {
	const (
		broadcast = moqt.BroadcastPath("/acme/cam")
		optIn     = moqt.BroadcastPath("/.hls/acme/cam")
		track     = moqt.TrackName("video")
	)
	tests := map[string]struct {
		relays int // 1: publishers and subscriber on one relay; 2: a leaf peered to an origin
	}{
		"one relay":          {relays: 1},
		"through a peer hop": {relays: 2},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cert, pool := chainCert(t)
			quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}
			origin := spinRelay(t, "origin", chainFreeAddr(t), cert, pool, quicCfg)
			edge := origin
			if tt.relays == 2 {
				edge = spinRelay(t, "leaf", chainFreeAddr(t), cert, pool, quicCfg)
				edge.Config.Peers = []Peer{{Address: origin.MOQServer.Addr}}
				go edge.ConnectPeers(ctx) //nolint:errcheck // ends with ctx
			}
			dial := func(mux *moqt.TrackMux, addr string) *moqt.Session {
				sess, err := (&moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: &quic.Config{EnableDatagrams: true}}).Dial(ctx, "moqt://"+addr, mux)
				require.NoError(t, err)
				t.Cleanup(func() { _ = sess.CloseWithError(moqt.NoError, "done") }) // not actionable: test teardown
				return sess
			}

			// The broadcast and its opt-in come from two sessions, as an
			// ingest and the app that opts it in do.
			media := moqt.NewTrackMux(moqt.NewHopID())
			media.PublishFunc(ctx, broadcast, func(tw *moqt.TrackWriter) {
				_ = writeGroup(ctx, tw, 1) // not actionable: the subscriber reports what it got
				<-tw.Context().Done()
			})
			dial(media, origin.MOQServer.Addr)
			opt := moqt.NewTrackMux(moqt.NewHopID())
			opt.PublishFunc(ctx, optIn, func(tw *moqt.TrackWriter) {
				if err := tw.Redirect(tw.Context(), moqt.Redirect{BroadcastPath: broadcast}); err != nil {
					tw.CloseWithError(moqt.SubscribeErrorCodeNotFound)
				}
			})
			dial(opt, origin.MOQServer.Addr)
			waitForHandler(t, edge, broadcast)
			waitForHandler(t, edge, optIn)

			sub := dial(moqt.NewTrackMux(0), edge.MOQServer.Addr)
			tr, err := sub.Subscribe(ctx, optIn, track, nil)
			require.NoError(t, err)
			defer tr.Close() //nolint:errcheck // test teardown

			assert.Equal(t, broadcast, tr.BroadcastPath, "followed the redirect to the broadcast")
			assert.Equal(t, track, tr.TrackName)
			gr, err := tr.AcceptGroup(ctx)
			require.NoError(t, err)
			assert.Equal(t, moqt.GroupSequence(1), gr.GroupSequence())
		})
	}
}
