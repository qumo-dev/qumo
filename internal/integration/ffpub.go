package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// errFFmpegAbsent is returned by [ffpubPublisher.Start] when ffmpeg is not on
// PATH. Tests that require ffmpeg skip when [ffpubAvailable] is false.
var errFFmpegAbsent = errors.New("ffpub: ffmpeg not found on PATH")

// ffpubAvailable reports whether ffmpeg is on PATH.
func ffpubAvailable() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// ffpubConfig parameterizes an ffmpeg RTMP/RTSP publish session driven by
// [ffpubPublisher].
type ffpubConfig struct {
	// URL is the RTMP or RTSP publish target (e.g. "rtmp://127.0.0.1:1935/live/test").
	URL string
	// BFrames enables B-frames in the encode. When false (the streaming
	// default) ffmpeg is launched with -bf 0.
	BFrames bool
	// Audio includes an AAC audio track synthesized from a 440 Hz sine tone.
	Audio bool
	// GOP is the keyframe interval (-g). Must be > 0.
	GOP int
	// Width and Height are the synthesized video dimensions. Must be > 0.
	Width, Height int
	// Framerate is the synthesized video frame rate. Must be > 0.
	Framerate int
	// Duration, if > 0, limits the publish time via ffmpeg's -t flag. If 0,
	// ffmpeg runs until its context is cancelled.
	Duration time.Duration
}

func (c ffpubConfig) validate() error {
	switch {
	case c.URL == "":
		return errors.New("ffpub: empty URL")
	case !strings.HasPrefix(c.URL, "rtmp:") && !strings.HasPrefix(c.URL, "rtsp:"):
		return fmt.Errorf("ffpub: URL must be rtmp: or rtsp:, got %q", c.URL)
	case c.GOP <= 0:
		return fmt.Errorf("ffpub: GOP must be > 0, got %d", c.GOP)
	case c.Width <= 0 || c.Height <= 0:
		return fmt.Errorf("ffpub: Width/Height must be > 0, got %dx%d", c.Width, c.Height)
	case c.Framerate <= 0:
		return fmt.Errorf("ffpub: Framerate must be > 0, got %d", c.Framerate)
	}
	return nil
}

// args builds the ffmpeg argument vector from a validated config.
func (c ffpubConfig) args() []string {
	a := []string{"-hide_banner", "-loglevel", "error", "-re"}
	if c.Duration > 0 {
		a = append(a, "-t", strconv.FormatFloat(c.Duration.Seconds(), 'f', -1, 64))
	}

	// Video source: testsrc2 produces a color-bar test pattern.
	a = append(a, "-f", "lavfi", "-i",
		fmt.Sprintf("testsrc2=size=%dx%d:rate=%d", c.Width, c.Height, c.Framerate))
	if c.Audio {
		a = append(a, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000")
	}

	// Video encode: OBS-like libx264 streaming settings.
	a = append(a, "-c:v", "libx264", "-g", strconv.Itoa(c.GOP))
	if !c.BFrames {
		a = append(a, "-bf", "0")
	}
	a = append(a, "-preset", "veryfast", "-tune", "zerolatency")

	if c.Audio {
		a = append(a, "-c:a", "aac", "-b:a", "128k")
	}

	// Output muxer + transport flags, selected by URL scheme. RTSP must use
	// TCP interleaving — qumo's RTSP ingest only accepts the interleaved
	// transport; ffmpeg defaults to UDP and would be rejected.
	format, extra := outputMuxer(c.URL)
	a = append(a, extra...)
	a = append(a, "-f", format, c.URL)
	return a
}

// outputMuxer returns the ffmpeg output muxer format and any required
// pre-output flags for the publish URL scheme: "rtsp" (with -rtsp_transport
// tcp) or "flv" for RTMP.
func outputMuxer(url string) (format string, extra []string) {
	if strings.HasPrefix(url, "rtsp:") {
		return "rtsp", []string{"-rtsp_transport", "tcp"}
	}
	return "flv", nil
}

// ffpubPublisher drives an ffmpeg subprocess publishing synthetic media to an
// RTMP or RTSP URL.
type ffpubPublisher struct {
	cfg ffpubConfig
	cmd *exec.Cmd
}

// newFFPub returns an [ffpubPublisher] for cfg. The config is not validated
// until [ffpubPublisher.Args] or [ffpubPublisher.Start] is called.
func newFFPub(cfg ffpubConfig) *ffpubPublisher {
	return &ffpubPublisher{cfg: cfg}
}

// Args returns the ffmpeg argument vector that [Start] would launch, without
// running ffmpeg. It validates the config first.
func (p *ffpubPublisher) Args() ([]string, error) {
	if err := p.cfg.validate(); err != nil {
		return nil, err
	}
	return p.cfg.args(), nil
}

// Start launches ffmpeg after verifying it is on PATH and the config is valid.
// The subprocess is killed when ctx is cancelled; the caller should [Wait] to
// reap the process.
func (p *ffpubPublisher) Start(ctx context.Context) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return errFFmpegAbsent
	}
	if err := p.cfg.validate(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", p.cfg.args()...)
	// Discard ffmpeg stderr so a busy -loglevel error stream does not spam
	// test output; callers needing it can wrap the Publisher later.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffpub: start ffmpeg: %w", err)
	}
	p.cmd = cmd
	return nil
}

// Wait blocks until ffmpeg exits and returns its error. Panics if [Start] was
// never called.
func (p *ffpubPublisher) Wait() error {
	if p.cmd == nil {
		panic("ffpub: Wait before Start")
	}
	return p.cmd.Wait()
}

// Process returns the underlying *os.Process, or nil if not started.
func (p *ffpubPublisher) Process() *os.Process {
	if p.cmd == nil {
		return nil
	}
	return p.cmd.Process
}
