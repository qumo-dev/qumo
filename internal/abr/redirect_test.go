package abr

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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/devcert"
)

// loopbackSession dials a MoQ server serving mux and returns the session,
// which follows redirects as the transcoder's does.
func loopbackSession(tb testing.TB, ctx context.Context, mux *moqt.TrackMux) *moqt.Session {
	tb.Helper()
	ca, err := devcert.NewCA("abr-test", time.Hour)
	require.NoError(tb, err)
	cert, err := ca.Issue(devcert.Leaf{CommonName: "localhost", DNSNames: []string{"localhost"}, Validity: time.Hour})
	require.NoError(tb, err)

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(tb, err)
	addr := pc.LocalAddr().String()
	require.NoError(tb, pc.Close())

	srv := &moqt.Server{
		Addr:       addr,
		TLSConfig:  &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{moqt.NextProtoMOQ}},
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

	dialer := &moqt.Dialer{TLSConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test-only self-signed cert
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

// A source track that redirects to another broadcast is not read: the
// transcoder may read every broadcast, so following it would publish
// someone else's media as the source's renditions.
func TestReadTrackRefusesARedirectedTrack(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	mux := moqt.NewTrackMux(0)
	mux.PublishFunc(ctx, "/victim/cam", func(tw *moqt.TrackWriter) {
		gw, err := tw.OpenGroup(tw.Context())
		if err != nil {
			return
		}
		frame := moqt.NewFrame(1)
		_, _ = frame.Write([]byte{0}) // not actionable: the test fails on any frame read
		_ = gw.WriteFrame(frame)      // not actionable: as above
		_ = gw.Close()                // not actionable: as above
		<-tw.Context().Done()
	})
	mux.PublishFunc(ctx, "/acme/cam", func(tw *moqt.TrackWriter) {
		if err := tw.Redirect(tw.Context(), moqt.Redirect{BroadcastPath: "/victim/cam", TrackName: tw.TrackName}); err != nil {
			tw.CloseWithError(moqt.SubscribeErrorCodeNotFound)
		}
	})
	p := &pipeline{sess: loopbackSession(t, ctx, mux), optIn: "/.abr/acme/cam", src: "/acme/cam"}

	err := p.readTrack(ctx, "video", func(moqt.GroupSequence) bool { return true },
		func(moqt.GroupSequence, int, *moqt.Frame) { t.Error("read a frame of another broadcast") }, func() {})

	mis, ok := errors.AsType[*MisdirectedError](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, moqt.BroadcastPath("/victim/cam"), mis.To)
}
