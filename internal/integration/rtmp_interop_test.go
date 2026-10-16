//go:build integration

package integration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/ffpub"
	"github.com/qumo-dev/qumo/internal/ingest"
	"github.com/stretchr/testify/require"
)

// TestRTMPInterop_Matrix drives ffmpeg as an RTMP publisher through a matrix of
// encoder configurations, subscribes back over MoQT (WebTransport), and asserts
// the gate. It stands up an in-process RTMP ingest + MoQT subscriber endpoint
// sharing one TrackMux (mirroring the standalone ingest, ingest.RunRTMP), then
// exercises the true RTMP transport-interop path (handshake, chunking, command
// sequencing). Subscribers use WebTransport — in this codebase native QUIC is
// relay-peer-only.
//
// Skips cleanly when ffmpeg is not on PATH. Run with:
//
//	go test -tags=integration ./internal/integration/...
func TestRTMPInterop_Matrix(t *testing.T) {
	if !ffpub.Available() {
		t.Skip("ffmpeg not on PATH")
	}

	mux := moqt.NewTrackMux(0)
	rtmpAddr, serveURL := setupRTMPPipeline(t, mux)

	// Matrix: B-frames on/off, audio on/off, two GOP lengths, two
	// resolution/framerate combos. Dimensions are intentionally not asserted —
	// the RTMP ingest does not parse width/height from the SPS yet (it leaves
	// them 0 in the catalog), so the gate's dimension check is disabled
	// (Width/Height = 0).
	const ctsWindow = 1_000_000 // 1s; allows B-frame composition reordering
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

			// Start ffmpeg publishing to this subtest's broadcast path.
			pub := ffpub.New(ffpub.Config{
				URL:       fmt.Sprintf("rtmp://%s%s", rtmpAddr, path),
				BFrames:   m.cfg.BFrames,
				Audio:     m.cfg.Audio,
				GOP:       m.cfg.GOP,
				Width:     m.cfg.Width,
				Height:    m.cfg.Height,
				Framerate: m.cfg.Framerate,
			})
			require.NoError(t, pub.Start(pubCtx))
			t.Cleanup(func() { _ = pub.Wait() })

			// Bound the whole collect so a misbehaving relay/ffmpeg can never
			// hang the subtest; the collector's internal catalog/group timeouts
			// bound the individual waits below this ceiling.
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

// setupRTMPPipeline stands up an in-process RTMP ingest server and a MoQT
// subscriber endpoint (WebTransport) sharing one TrackMux — the same wiring as
// the standalone ingest (ingest.RunRTMP). Returns the RTMP address
// (host:port) and the WebTransport subscriber URL (https://host:port/).
func setupRTMPPipeline(t *testing.T, mux *moqt.TrackMux) (rtmpAddr, serveURL string) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// RTMP ingest server (plain TCP) on an ephemeral loopback port.
	rtmpLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	rtmpAddr = rtmpLn.Addr().String()
	require.NoError(t, rtmpLn.Close())
	rtmpSrv := ingest.NewRTMPServer(ingest.RTMPConfig{Addr: rtmpAddr, TrackMux: mux})
	rtmpErr := make(chan error, 1)
	go func() { rtmpErr <- rtmpSrv.ListenAndServe(ctx) }()

	// Gate ffmpeg on RTMP readiness. ffmpeg has no -reconnect, so connecting
	// before the listener is bound fails fast and publishes nothing; and a
	// bind failure (e.g. a TOCTOU port grab between the probe close and
	// rtmp.Listen) would otherwise surface only as an opaque collect timeout.
	// Dial the TCP port, and if the server exited before accepting, fail with
	// the cause rather than retrying to the timeout.
	require.Eventually(t, func() bool {
		c, derr := net.DialTimeout("tcp", rtmpAddr, 200*time.Millisecond)
		if derr == nil {
			_ = c.Close()
			return true
		}
		select {
		case err := <-rtmpErr:
			if err != nil {
				t.Fatalf("RTMP ingest failed to start: %v", err)
			}
		default:
		}
		return false
	}, 5*time.Second, 50*time.Millisecond, "RTMP ingest never became reachable")

	// MoQT subscriber endpoint over WebTransport (HTTP/3), serving subscribers
	// from the shared TrackMux. Mirrors ingest.RunRTMP exactly.
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

	// Wait until the subscriber endpoint accepts WebTransport/MoQT sessions.
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

	return rtmpAddr, serveURL
}

func subscriberTLS(t *testing.T) *tls.Config {
	t.Helper()
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // self-signed in-process test cert
		MinVersion:         tls.VersionTLS13,
	}
}

// createTempCert generates an ephemeral self-signed cert + key and returns
// their file paths (moqt.Server.ListenAndServeTLS takes file paths).
func createTempCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "interop-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	require.NoError(t, err)

	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certFile,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	keyBytes, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(keyFile,
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}), 0o600))
	return certFile, keyFile
}

// freeUDPPort returns an ephemeral UDP port on loopback.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	require.NoError(t, err)
	c, err := net.ListenUDP("udp", addr)
	require.NoError(t, err)
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}
