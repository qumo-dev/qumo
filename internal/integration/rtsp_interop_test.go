//go:build integration

package integration

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/ffpub"
	"github.com/qumo-dev/qumo/internal/ingest"
	"github.com/stretchr/testify/require"
)

// TestRTSPInterop_Matrix is the RTSP analogue of TestRTMPInterop_Matrix: it
// drives ffmpeg as an RTSP publisher (ANNOUNCE/RECORD, interleaved TCP) through
// a matrix of encoder configurations, subscribes back over MoQT (WebTransport),
// and asserts the gate. It stands up an in-process RTSP ingest + MoQT subscriber
// endpoint sharing one TrackMux. Skips cleanly when ffmpeg is not on PATH.
//
// Run with: go test -tags=integration ./internal/integration/...
func TestRTSPInterop_Matrix(t *testing.T) {
	if !ffpub.Available() {
		t.Skip("ffmpeg not on PATH")
	}

	mux := moqt.NewTrackMux(0)
	rtspAddr, serveURL := setupRTSPPipeline(t, mux)

	// Dimensions are intentionally not asserted: the RTSP ingest hardcodes
	// 1920x1080 in the catalog (it does not parse SPS dimensions), so the gate's
	// dimension check is disabled (Width/Height = 0).
	//
	// CTS window bounds the PTS regression the gate tolerates between consecutive
	// decode-ordered frames (B-frame composition reorder). 1 s is generous for
	// the no-B-frame test vectors here (ffpub passes `-bf 0` + `-tune zerolatency`
	// for cases that leave BFrames unset, so PTS == DTS and should be monotonic).
	const ctsWindow = 1_000_000
	matrix := []struct {
		name string
		cfg  ffpub.Config
		exp  Expectations
	}{
		{
			name: "no_bframes_no_audio",
			cfg:  ffpub.Config{GOP: 30, Width: 320, Height: 240, Framerate: 30},
			exp:  Expectations{WantVideo: true, VideoCodecPrefix: "avc1.", MinVideoFrames: 5, MinKeyframes: 1, RequireInitData: true, MaxCTSWindowUS: ctsWindow},
		},
		{
			name: "bframes_audio",
			cfg:  ffpub.Config{BFrames: true, Audio: true, GOP: 30, Width: 320, Height: 240, Framerate: 30},
			exp:  Expectations{WantVideo: true, WantAudio: true, VideoCodecPrefix: "avc1.", AudioCodecPrefix: "mp4a.40.", MinVideoFrames: 5, MinAudioFrames: 2, MinKeyframes: 1, RequireInitData: true, MaxCTSWindowUS: ctsWindow},
		},
		{
			name: "gop60_720p30",
			cfg:  ffpub.Config{GOP: 60, Width: 1280, Height: 720, Framerate: 30},
			exp:  Expectations{WantVideo: true, VideoCodecPrefix: "avc1.", MinVideoFrames: 5, MinKeyframes: 1, RequireInitData: true, MaxCTSWindowUS: ctsWindow},
		},
		{
			name: "gop15_320x240_15fps",
			cfg:  ffpub.Config{GOP: 15, Width: 320, Height: 240, Framerate: 15},
			exp:  Expectations{WantVideo: true, VideoCodecPrefix: "avc1.", MinVideoFrames: 5, MinKeyframes: 1, RequireInitData: true, MaxCTSWindowUS: ctsWindow},
		},
	}

	for _, m := range matrix {
		t.Run(m.name, func(t *testing.T) {
			path := "/live/" + m.name
			pubCtx, cancelPub := context.WithCancel(context.Background())
			defer cancelPub()

			pub := ffpub.New(ffpub.Config{
				URL:       fmt.Sprintf("rtsp://%s%s", rtspAddr, path),
				BFrames:   m.cfg.BFrames,
				Audio:     m.cfg.Audio,
				GOP:       m.cfg.GOP,
				Width:     m.cfg.Width,
				Height:    m.cfg.Height,
				Framerate: m.cfg.Framerate,
			})
			require.NoError(t, pub.Start(pubCtx))
			t.Cleanup(func() { _ = pub.Wait() })

			collectCtx, cancelCollect := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancelCollect()
			obs, err := (&Collector{
				Dialer:       &moqt.Dialer{TLSConfig: subscriberTLS(t)},
				URL:          serveURL,
				Path:         moqt.BroadcastPath(path),
				MaxGroups:    3,
				GroupTimeout: 4 * time.Second,
				WantTracks:   m.exp.TrackNames(),
			}).Collect(collectCtx)
			require.NoError(t, err, "collect failed")
			cancelPub()

			v := Evaluate(obs, m.exp)
			require.Truef(t, v.Pass, "gate failed:\n%s", v.String())
		})
	}
}

// setupRTSPPipeline stands up an in-process RTSP ingest server and a MoQT
// subscriber endpoint (WebTransport) sharing one TrackMux. Returns the RTSP
// address (host:port) and the WebTransport subscriber URL (https://host:port/).
func setupRTSPPipeline(t *testing.T, mux *moqt.TrackMux) (rtspAddr, serveURL string) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// RTSP ingest server (plain TCP) on an ephemeral loopback port.
	rtspLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	rtspAddr = rtspLn.Addr().String()
	require.NoError(t, rtspLn.Close())
	rtspSrv := ingest.NewRTSPServer(ingest.RTSPConfig{Addr: rtspAddr, TrackMux: mux})
	go func() { _ = rtspSrv.ListenAndServe(ctx) }()

	// Gate ffmpeg on RTSP readiness. Unlike the RTMP listener, the RTSP
	// listener does no handshake in Accept and dispatches each connection to
	// its own goroutine, so a probe connect/close cannot tear it down.
	require.Eventually(t, func() bool {
		c, derr := net.DialTimeout("tcp", rtspAddr, 200*time.Millisecond)
		if derr == nil {
			_ = c.Close()
			return true
		}
		return false
	}, 5*time.Second, 50*time.Millisecond, "RTSP ingest never became reachable")

	// MoQT subscriber endpoint over WebTransport, serving from the shared
	// TrackMux (same shape as the RTMP pipeline / ingest.RunRTMP).
	certFile, keyFile := createTempCert(t)
	wtHandler := &moqt.WebTransportHandler{
		TrackMux:    mux,
		CheckOrigin: func(*http.Request) bool { return true }, // test-only permissive
		Handler: moqt.HandleFunc(func(sess *moqt.Session) {
			defer sess.CloseWithError(moqt.NoError, moqt.NoError.String())
			<-sess.Context().Done()
		}),
	}
	httpMux := http.NewServeMux()
	httpMux.Handle("/", wtHandler)

	quicAddr := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))
	moqSrv := &moqt.Server{
		Addr:               quicAddr,
		WebTransportServer: moqt.NewWebTransportServer(httpMux),
		TrackMux:           mux,
	}
	go func() { _ = moqSrv.ListenAndServeTLS(certFile, keyFile) }()
	t.Cleanup(func() {
		shutCtx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = moqSrv.Shutdown(shutCtx)
	})

	serveURL = "https://" + quicAddr + "/"
	require.Eventually(t, func() bool {
		probe := &moqt.Dialer{TLSConfig: subscriberTLS(t)}
		pctx, c := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer c()
		sess, derr := probe.Dial(pctx, serveURL, moqt.NewTrackMux(0))
		if derr != nil {
			return false
		}
		_ = sess.CloseWithError(moqt.NoError, "probe")
		return true
	}, 5*time.Second, 100*time.Millisecond, "subscriber endpoint never became reachable")

	return rtspAddr, serveURL
}
