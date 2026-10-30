//go:build integration

package relay

import (
	"context"
	"crypto/tls"
	"errors"
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

// A relay does not send its subscribers to another relay a publisher names:
// it refuses such a redirect. Only a server may send one, so the publisher
// here is a plain gomoqt server the relay peers with.
func TestRelay_RefusesRedirectToAnotherRelay(t *testing.T) {
	const optIn = moqt.BroadcastPath("/.hls/acme/cam")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cert, pool := chainCert(t)
	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}

	// The publisher: a gomoqt server whose opt-in redirects to another host.
	pubMux := moqt.NewTrackMux(moqt.NewHopID())
	pubMux.PublishFunc(ctx, optIn, func(tw *moqt.TrackWriter) {
		if err := tw.Redirect(tw.Context(), moqt.Redirect{URI: "moqt://elsewhere.example/", BroadcastPath: "/acme/cam"}); err != nil {
			tw.CloseWithError(moqt.SubscribeErrorCodeNotFound)
		}
	})
	pubAddr := chainFreeAddr(t)
	pub := &moqt.Server{
		Addr: pubAddr,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert}, NextProtos: []string{moqt.NextProtoMOQ}, MinVersion: tls.VersionTLS13,
		},
		QUICConfig: quicCfg,
		TrackMux:   pubMux,
		Handler:    moqt.HandleFunc(func(sess *moqt.Session) { <-sess.Context().Done() }),
	}
	go func() { _ = pub.ListenAndServe() }() // not actionable: ends with Close
	t.Cleanup(func() { _ = pub.Close() })    // not actionable: test teardown
	chainWaitReachable(t, pubAddr, pool, quicCfg)

	leaf := spinRelay(t, "leaf", chainFreeAddr(t), cert, pool, quicCfg)
	leaf.Config.Peers = []Peer{{Address: pubAddr}}
	go leaf.ConnectPeers(ctx) //nolint:errcheck // ends with ctx
	waitForHandler(t, leaf, optIn)

	sess, err := (&moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: &quic.Config{EnableDatagrams: true}}).Dial(ctx, "moqt://"+leaf.MOQServer.Addr, moqt.NewTrackMux(0))
	require.NoError(t, err)
	defer sess.CloseWithError(moqt.NoError, "done") //nolint:errcheck // test teardown

	_, err = sess.Subscribe(ctx, optIn, "video", nil)

	_, redirected := errors.AsType[*moqt.RedirectError](err)
	assert.False(t, redirected, "the subscriber is not sent to the other relay")
	subErr, ok := errors.AsType[*moqt.SubscribeError](err)
	require.True(t, ok, "want a *moqt.SubscribeError, got %v", err)
	assert.Equal(t, moqt.SubscribeErrorCodeNotFound, subErr.SubscribeErrorCode())
}

// A relay remembers a publisher's redirect for as long as the announcement:
// later subscribers get it without the publisher being asked again. A new
// announcement of the path starts afresh, so a publisher that would redirect
// elsewhere announces again.
func TestRelay_CachesRedirect(t *testing.T) {
	const (
		optIn = moqt.BroadcastPath("/.hls/acme/cam")
		track = moqt.TrackName("video")
	)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cert, pool := chainCert(t)
	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}
	r := spinRelay(t, "origin", chainFreeAddr(t), cert, pool, quicCfg)
	dial := func(mux *moqt.TrackMux) *moqt.Session {
		sess, err := (&moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: &quic.Config{EnableDatagrams: true}}).Dial(ctx, "moqt://"+r.MOQServer.Addr, mux)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sess.CloseWithError(moqt.NoError, "done") }) // not actionable: test teardown
		return sess
	}
	media := moqt.NewTrackMux(moqt.NewHopID())
	for _, path := range []moqt.BroadcastPath{"/acme/cam", "/acme/cam-hd"} {
		media.PublishFunc(ctx, path, func(tw *moqt.TrackWriter) {
			_ = writeGroup(ctx, tw, 1) // not actionable: the subscriber reports what it got
			<-tw.Context().Done()
		})
	}
	dial(media)
	asked := make(chan struct{}, 16)
	optInTo := func(to moqt.BroadcastPath) *moqt.TrackMux {
		mux := moqt.NewTrackMux(moqt.NewHopID())
		mux.PublishFunc(ctx, optIn, func(tw *moqt.TrackWriter) {
			asked <- struct{}{}
			if err := tw.Redirect(tw.Context(), moqt.Redirect{BroadcastPath: to}); err != nil {
				tw.CloseWithError(moqt.SubscribeErrorCodeNotFound)
			}
		})
		return mux
	}
	sub := dial(moqt.NewTrackMux(0))
	subscribe := func() moqt.BroadcastPath {
		tr, err := sub.Subscribe(ctx, optIn, track, nil)
		require.NoError(t, err)
		defer tr.Close() //nolint:errcheck // test teardown
		return tr.BroadcastPath
	}

	first := dial(optInTo("/acme/cam"))
	waitForHandler(t, r, optIn)
	got := []moqt.BroadcastPath{subscribe(), subscribe(), subscribe()}

	assert.Equal(t, []moqt.BroadcastPath{"/acme/cam", "/acme/cam", "/acme/cam"}, got)
	assert.Len(t, asked, 1, "the publisher is asked once")

	// The opt-in is announced again, redirecting elsewhere.
	require.NoError(t, first.CloseWithError(moqt.NoError, "re-announce"))
	require.Eventually(t, func() bool {
		h, _ := r.TrackMux.TrackHandler(optIn)
		return h == nil
	}, 10*time.Second, 20*time.Millisecond, "the first announcement ends")
	dial(optInTo("/acme/cam-hd"))
	waitForHandler(t, r, optIn)

	assert.Equal(t, moqt.BroadcastPath("/acme/cam-hd"), subscribe(), "the new announcement's redirect")
	assert.Len(t, asked, 2)
}
