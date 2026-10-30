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

	dialer := &moqt.Dialer{TLSConfig: &tls.Config{InsecureSkipVerify: true}, CheckRedirect: stopRedirect} //nolint:gosec // test-only self-signed cert
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
	mux.PublishFunc(ctx, "/.hls/acme/cam", func(tw *moqt.TrackWriter) {
		if err := tw.Redirect(tw.Context(), moqt.Redirect{BroadcastPath: "/acme/cam"}); err != nil {
			tw.CloseWithError(moqt.SubscribeErrorCodeNotFound)
		}
	})
	mux.PublishFunc(ctx, "/legacy/cam", serveCatalog(t, catalog))
	mux.Publish(ctx, "/.hls/legacy/cam", moqt.NotFoundTrackHandler)
	// An opt-in that redirects to a broadcast not published yet, while its
	// plain path is published: the egress must not take the plain path.
	mux.PublishFunc(ctx, "/.hls/early/cam", func(tw *moqt.TrackWriter) {
		if err := tw.Redirect(tw.Context(), moqt.Redirect{BroadcastPath: "/early/cam-hd"}); err != nil {
			tw.CloseWithError(moqt.SubscribeErrorCodeNotFound)
		}
	})
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
