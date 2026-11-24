package integration

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func baseFFPubCfg(url string) ffpubConfig {
	return ffpubConfig{
		URL:       url,
		GOP:       30,
		Width:     1280,
		Height:    720,
		Framerate: 30,
	}
}

// containsAll reports whether the joined arg vector contains every substring.
func containsAll(args []string, want ...string) bool {
	joined := strings.Join(args, "\x00")
	for _, w := range want {
		if !strings.Contains(joined, w) {
			return false
		}
	}
	return true
}

func TestFFPub_Args(t *testing.T) {
	t.Run("default (no B-frames, no audio)", func(t *testing.T) {
		args, err := newFFPub(baseFFPubCfg("rtmp://example/live/test")).Args()
		require.NoError(t, err)
		assert.Contains(t, args, "-bf", "B-frames disabled via -bf 0")
		assert.Contains(t, args, "0")
		assert.True(t, containsAll(args,
			"testsrc2=size=1280x720:rate=30", "-g", "30",
			"-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency",
			"-f", "flv", "rtmp://example/live/test"))
		// No audio args.
		assert.NotContains(t, strings.Join(args, "\x00"), "sine")
		assert.NotContains(t, strings.Join(args, "\x00"), "-c:a")
	})

	t.Run("B-frames enabled omits -bf", func(t *testing.T) {
		cfg := baseFFPubCfg("rtmp://x/y")
		cfg.BFrames = true
		args, err := newFFPub(cfg).Args()
		require.NoError(t, err)
		assert.NotContains(t, args, "-bf", "no -bf flag when B-frames enabled")
	})

	t.Run("audio adds sine source and AAC encoder", func(t *testing.T) {
		cfg := baseFFPubCfg("rtmp://x/y")
		cfg.Audio = true
		args, err := newFFPub(cfg).Args()
		require.NoError(t, err)
		assert.True(t, containsAll(args,
			"sine=frequency=440:sample_rate=48000", "-c:a", "aac", "-b:a", "128k"))
	})

	t.Run("GOP and resolution/framerate reflected", func(t *testing.T) {
		cfg := ffpubConfig{URL: "rtmp://x/y", GOP: 60, Width: 1920, Height: 1080, Framerate: 25}
		args, err := newFFPub(cfg).Args()
		require.NoError(t, err)
		assert.True(t, containsAll(args, "testsrc2=size=1920x1080:rate=25", "-g", "60"))
	})

	t.Run("duration adds -t", func(t *testing.T) {
		cfg := baseFFPubCfg("rtmp://x/y")
		cfg.Duration = 2500 * time.Millisecond
		args, err := newFFPub(cfg).Args()
		require.NoError(t, err)
		// Find -t and check its value.
		i := indexOf(args, "-t")
		require.GreaterOrEqual(t, i, 0)
		assert.Equal(t, "2.5", args[i+1])
	})

	t.Run("rtsp URL uses rtsp muxer + tcp transport", func(t *testing.T) {
		cfg := baseFFPubCfg("rtsp://x:8554/y")
		args, err := newFFPub(cfg).Args()
		require.NoError(t, err)
		// RTSP must force TCP: qumo's RTSP ingest only accepts interleaved TCP.
		i := indexOf(args, "-rtsp_transport")
		require.GreaterOrEqual(t, i, 0)
		assert.Equal(t, "tcp", args[i+1])
		assert.True(t, containsAll(args, "-f", "rtsp", "rtsp://x:8554/y"))
		assert.NotContains(t, strings.Join(args, "\x00"), "-flv")
	})
}

func TestFFPub_Args_Validation(t *testing.T) {
	tests := map[string]ffpubConfig{
		"empty URL":      {GOP: 30, Width: 320, Height: 240, Framerate: 30},
		"invalid scheme": {URL: "udp://x/y", GOP: 30, Width: 320, Height: 240, Framerate: 30},
		"zero GOP":       {URL: "rtmp://x/y", Width: 320, Height: 240, Framerate: 30},
		"zero width":     {URL: "rtmp://x/y", GOP: 30, Height: 240, Framerate: 30},
		"zero framerate": {URL: "rtmp://x/y", GOP: 30, Width: 320, Height: 240},
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := newFFPub(cfg).Args()
			assert.Error(t, err)
		})
	}
}

// TestFFPub_StartStop exercises the real ffmpeg subprocess lifecycle:
// ffmpeg connects to a local TCP sink (RTMP URL), runs, and is killed cleanly
// on context cancellation. Skips when ffmpeg is not on PATH.
func TestFFPub_StartStop(t *testing.T) {
	if !ffpubAvailable() {
		t.Skip("ffmpeg not on PATH")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	// Accept and drain so ffmpeg's TCP connect succeeds and it begins the
	// RTMP handshake (we do not complete it; ffmpeg blocks waiting, which is
	// fine — the test only needs the process alive and connected).
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go io.Copy(io.Discard, conn)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	url := "rtmp://" + ln.Addr().String() + "/live/test"
	p := newFFPub(baseFFPubCfg(url))

	require.NoError(t, p.Start(ctx))
	proc := p.Process()
	require.NotNil(t, proc)

	// Give ffmpeg time to start up and connect.
	time.Sleep(400 * time.Millisecond)

	// Cancel must tear down the subprocess promptly.
	cancel()
	waitErr := make(chan error, 1)
	go func() { waitErr <- p.Wait() }()
	select {
	case <-waitErr:
		// Process exited (killed by context cancellation). Expected.
	case <-time.After(5 * time.Second):
		t.Fatalf("ffmpeg did not exit within 5s of context cancellation")
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}
