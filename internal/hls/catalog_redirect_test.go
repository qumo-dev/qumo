package hls

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/gomoqt/msf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/devcert"
	"github.com/qumo-dev/qumo/internal/relay"
)

// loopbackPublisher serves mux on a loopback native QUIC address and returns
// a session dialed to it as the egress dials the relay: it stops at redirects
// and subscribeCatalog follows them.
func loopbackPublisher(tb testing.TB, ctx context.Context, mux *moqt.TrackMux) *moqt.Session {
	tb.Helper()
	ca, err := devcert.NewCA("hls-test", time.Hour)
	require.NoError(tb, err)
	cert, err := ca.Issue(devcert.Leaf{CommonName: "localhost", DNSNames: []string{"localhost"}, Validity: time.Hour})
	require.NoError(tb, err)

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(tb, err)
	addr := pc.LocalAddr().String()
	require.NoError(tb, pc.Close())

	srv := &moqt.Server{
		Addr: addr,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{moqt.NextProtoMOQ},
		},
		QUICConfig: &quic.Config{EnableDatagrams: true},
		TrackMux:   mux,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Handler:    moqt.HandleFunc(func(sess *moqt.Session) { <-sess.Context().Done() }),
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, moqt.ErrServerClosed) {
			tb.Logf("server: %v", err)
		}
	}()
	tb.Cleanup(func() { _ = srv.Close() }) // not actionable: test teardown

	dialer := &moqt.Dialer{TLSConfig: &tls.Config{InsecureSkipVerify: true}, CheckRedirect: relay.StopRedirect} //nolint:gosec // test-only self-signed cert
	var sess *moqt.Session
	require.Eventually(tb, func() bool {
		s, err := dialer.Dial(ctx, "moqt://"+addr+"/", nil)
		if err != nil {
			return false
		}
		sess = s
		return true
	}, 5*time.Second, 50*time.Millisecond)
	tb.Cleanup(func() { _ = sess.CloseWithError(moqt.NoError, "done") }) // not actionable: test teardown
	return sess
}

// serveCatalog serves catalog as one group on the catalog track.
func serveCatalog(tb testing.TB, catalog msf.Catalog) func(*moqt.TrackWriter) {
	tb.Helper()
	data, err := catalog.MarshalJSON()
	require.NoError(tb, err)
	return func(tw *moqt.TrackWriter) {
		if tw.TrackName != msf.DefaultCatalogTrackName {
			tw.CloseWithError(moqt.SubscribeErrorCodeNotFound)
			return
		}
		gw, err := tw.OpenGroup(tw.Context())
		if err != nil {
			return
		}
		frame := moqt.NewFrame(len(data))
		_, _ = frame.Write(data) // not actionable: the subscriber reports what it read
		_ = gw.WriteFrame(frame) // not actionable: as above
		_ = gw.Close()           // not actionable: as above
		<-tw.Context().Done()
	}
}

// The egress subscribes to an opt-in's catalog at the announced path: an
// opt-in that redirects sends it to the broadcast, and one from a publisher
// that predates the redirect, answering NOT_FOUND, is read at the broadcast's
// own path. Either way the catalog comes back with the path it came from.
func TestFetchCatalog_OptIn(t *testing.T) {
	w, h := int64(1280), int64(720)
	catalog := msf.Catalog{Tracks: []msf.Track{{
		Name: "video", Role: msf.RoleVideo, Packaging: msf.PackagingLOC,
		Codec: "vp09.00.10.08", Width: &w, Height: &h,
	}}}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	mux := moqt.NewTrackMux(0)
	mux.PublishFunc(ctx, "/acme/cam", serveCatalog(t, catalog))
	mux.Publish(ctx, "/.hls/acme/cam", OptInHandler("/acme/cam"))
	mux.PublishFunc(ctx, "/legacy/cam", serveCatalog(t, catalog))
	mux.Publish(ctx, "/.hls/legacy/cam", moqt.NotFoundTrackHandler)
	// An opt-in that redirects to a broadcast not published yet, while its
	// plain path is published: the egress must not take the plain path.
	mux.Publish(ctx, "/.hls/early/cam", OptInHandler("/early/cam-hd"))
	mux.PublishFunc(ctx, "/early/cam", serveCatalog(t, catalog))
	mux.Publish(ctx, "/.hls/gone/cam", moqt.NotFoundTrackHandler)
	sess := loopbackPublisher(t, ctx, mux)

	tests := map[string]struct {
		optIn, broadcast string
		wantSource       moqt.BroadcastPath
		wantErr          bool
	}{
		"an opt-in that redirects":               {optIn: "/.hls/acme/cam", broadcast: "/acme/cam", wantSource: "/acme/cam"},
		"an opt-in from before that":             {optIn: "/.hls/legacy/cam", broadcast: "/legacy/cam", wantSource: "/legacy/cam"},
		"a redirect to an unpublished broadcast": {optIn: "/.hls/early/cam", broadcast: "/early/cam", wantErr: true},
		"neither the opt-in nor its broadcast":   {optIn: "/.hls/gone/cam", broadcast: "/gone/cam", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, source, err := fetchCatalog(ctx, sess, tt.optIn, tt.broadcast)

			if tt.wantErr {
				subErr, ok := errors.AsType[*moqt.SubscribeError](err)
				require.True(t, ok, "want a *moqt.SubscribeError, got %v", err)
				assert.Equal(t, moqt.SubscribeErrorCodeNotFound, subErr.SubscribeErrorCode())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantSource, source)
			require.Len(t, got.Tracks, 1)
			assert.Equal(t, "video", got.Tracks[0].Name)
		})
	}
}

// redirectTo serves every subscription with a redirect to to.
func redirectTo(to moqt.Redirect) moqt.TrackHandler {
	return moqt.TrackHandlerFunc(func(tw *moqt.TrackWriter) {
		if err := tw.Redirect(tw.Context(), to); err != nil {
			tw.CloseWithError(moqt.SubscribeErrorCodeNotFound)
		}
	})
}

// subscribeFollowing follows redirects on the session, up to two, and reports
// how many; a redirect to another relay or one hop too many is not retried.
func TestSubscribeFollowing(t *testing.T) {
	w, h := int64(1280), int64(720)
	catalog := msf.Catalog{Tracks: []msf.Track{{
		Name: "video", Role: msf.RoleVideo, Packaging: msf.PackagingLOC,
		Codec: "vp09.00.10.08", Width: &w, Height: &h,
	}}}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	mux := moqt.NewTrackMux(0)
	mux.PublishFunc(ctx, "/real", serveCatalog(t, catalog))
	mux.Publish(ctx, "/one", redirectTo(moqt.Redirect{BroadcastPath: "/real"}))
	mux.Publish(ctx, "/two", redirectTo(moqt.Redirect{BroadcastPath: "/one"}))
	mux.Publish(ctx, "/loop-a", redirectTo(moqt.Redirect{BroadcastPath: "/loop-b"}))
	mux.Publish(ctx, "/loop-b", redirectTo(moqt.Redirect{BroadcastPath: "/loop-a"}))
	sess := loopbackPublisher(t, ctx, mux)

	tests := map[string]struct {
		path     moqt.BroadcastPath
		wantHops int
		wantErr  error
	}{
		"no redirect":   {path: "/real", wantHops: 0},
		"one redirect":  {path: "/one", wantHops: 1},
		"two redirects": {path: "/two", wantHops: 2},
		"a loop":        {path: "/loop-a", wantHops: 2, wantErr: moqt.ErrTooManyRedirects},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			tr, hops, err := subscribeFollowing(ctx, sess, tt.path, msf.DefaultCatalogTrackName)

			assert.Equal(t, tt.wantHops, hops)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				assert.ErrorIs(t, err, errNotRetryable)
				return
			}
			require.NoError(t, err)
			defer tr.Close() //nolint:errcheck // test teardown
			assert.Equal(t, moqt.BroadcastPath("/real"), tr.BroadcastPath)
		})
	}
}

// An opt-in that answered NOT_FOUND once is a publisher from before the
// redirect: a retry asks its broadcast directly, not the opt-in again.
func TestOptInCatalog_RemembersAnOlderPublisher(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	optInAsked := make(chan struct{}, 4)
	mux := moqt.NewTrackMux(0)
	mux.PublishFunc(ctx, "/.hls/legacy/cam", func(tw *moqt.TrackWriter) {
		optInAsked <- struct{}{}
		tw.CloseWithError(moqt.SubscribeErrorCodeNotFound)
	})
	sess := loopbackPublisher(t, ctx, mux)
	c := &optInCatalog{optIn: "/.hls/legacy/cam", broadcast: "/legacy/cam"}

	_, _, firstErr := c.fetch(ctx, sess)
	_, _, secondErr := c.fetch(ctx, sess)

	require.Error(t, firstErr, "the broadcast is not published")
	require.Error(t, secondErr)
	assert.True(t, c.legacy)
	assert.Len(t, optInAsked, 1, "the opt-in is asked once")
}
