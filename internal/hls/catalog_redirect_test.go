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
	"github.com/qumo-dev/qumo/internal/hlsoptin"
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
	mux.Publish(ctx, "/.hls/acme/cam", hlsoptin.Handler("/acme/cam"))
	mux.PublishFunc(ctx, "/legacy/cam", serveCatalog(t, catalog))
	mux.Publish(ctx, "/.hls/legacy/cam", moqt.NotFoundTrackHandler)
	// An opt-in that redirects to its broadcast before that is published: an
	// error to retry, not an opt-in from before the redirect.
	mux.Publish(ctx, "/.hls/early/cam", hlsoptin.Handler("/early/cam"))
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
			got, source, err := (&optInCatalog{optIn: tt.optIn, broadcast: tt.broadcast}).fetch(ctx, sess)

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
	serve := serveCatalog(t, catalog)
	// /real redirects among its own tracks: hop2 → hop1 → the catalog, and
	// loop → loop.
	within := map[moqt.TrackName]moqt.TrackName{"hop2": "hop1", "hop1": msf.DefaultCatalogTrackName, "loop": "loop"}
	mux := moqt.NewTrackMux(0)
	mux.PublishFunc(ctx, "/real", func(tw *moqt.TrackWriter) {
		if next, ok := within[tw.TrackName]; ok {
			redirectTo(moqt.Redirect{BroadcastPath: "/real", TrackName: next}).ServeTrack(tw)
			return
		}
		serve(tw)
	})
	mux.Publish(ctx, "/.hls/real", redirectTo(moqt.Redirect{BroadcastPath: "/real"}))
	mux.Publish(ctx, "/.hls/away", redirectTo(moqt.Redirect{BroadcastPath: "/other/secret"}))
	sess := loopbackPublisher(t, ctx, mux)

	tests := map[string]struct {
		path      moqt.BroadcastPath
		name      moqt.TrackName
		wantHops  int
		wantErr   error
		wantOther bool // refused for leaving the broadcast
	}{
		"no redirect":              {path: "/real", name: msf.DefaultCatalogTrackName, wantHops: 0},
		"to the broadcast":         {path: "/.hls/real", name: msf.DefaultCatalogTrackName, wantHops: 1},
		"two within the broadcast": {path: "/real", name: "hop2", wantHops: 2},
		"a loop":                   {path: "/real", name: "loop", wantHops: 2, wantErr: moqt.ErrTooManyRedirects},
		"outside the broadcast":    {path: "/.hls/away", name: msf.DefaultCatalogTrackName, wantHops: 0, wantOther: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			tr, hops, err := subscribeFollowing(ctx, sess, tt.path, tt.name, "/real")

			assert.Equal(t, tt.wantHops, hops)
			if tt.wantErr != nil || tt.wantOther {
				assert.ErrorIs(t, err, errNotRetryable, "a retry cannot change the answer")
				if tt.wantErr != nil {
					assert.ErrorIs(t, err, tt.wantErr)
				}
				assert.Nil(t, tr)
				return
			}
			require.NoError(t, err)
			defer tr.Close() //nolint:errcheck // test teardown
			assert.Equal(t, moqt.BroadcastPath("/real"), tr.BroadcastPath)
		})
	}
}

// A tenant's opt-in that redirects to someone else's broadcast does not get
// the egress to read it: the catalog fetch fails, once, without retries.
func TestFetchCatalogWithRetry_RefusesARedirectElsewhere(t *testing.T) {
	w, h := int64(1280), int64(720)
	catalog := msf.Catalog{Tracks: []msf.Track{{
		Name: "video", Role: msf.RoleVideo, Packaging: msf.PackagingLOC,
		Codec: "vp09.00.10.08", Width: &w, Height: &h,
	}}}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	asked := make(chan struct{}, 16)
	mux := moqt.NewTrackMux(0)
	mux.PublishFunc(ctx, "/other/secret", serveCatalog(t, catalog))
	mux.PublishFunc(ctx, "/.hls/acme/x", func(tw *moqt.TrackWriter) {
		asked <- struct{}{}
		redirectTo(moqt.Redirect{BroadcastPath: "/other/secret"}).ServeTrack(tw)
	})
	sess := loopbackPublisher(t, ctx, mux)

	_, _, err := fetchCatalogWithRetry(ctx, sess, "/.hls/acme/x", "/acme/x")

	assert.ErrorIs(t, err, errNotRetryable)
	assert.Len(t, asked, 1, "not retried")
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

func TestSameBroadcast(t *testing.T) {
	tests := map[string]struct {
		a, b moqt.BroadcastPath
		want bool
	}{
		"the same":              {a: "/acme/cam", b: "/acme/cam", want: true},
		"a trailing slash":      {a: "/acme/cam/", b: "/acme/cam", want: true},
		"a doubled slash":       {a: "/acme//cam", b: "/acme/cam", want: true},
		"another broadcast":     {a: "/other/cam", b: "/acme/cam", want: false},
		"beneath the broadcast": {a: "/acme/cam/hd", b: "/acme/cam", want: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, sameBroadcast(tt.a, tt.b))
		})
	}
}
