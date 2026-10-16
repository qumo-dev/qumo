//go:build integration

package integration

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/ffpub"
)

// TestRTSP_PTSMonotonic is the regression guard for #229.
//
// ffmpeg's RTSP muxer intermittently emits several IDR NALUs back-to-back at
// the same presentation timestamp within one access unit. The ingest used to
// open a fresh MoQT group on every keyframe NALU, so this produced rapid
// micro-group churn that the relay ring / a bounded collector window delivered
// out of order — observed downstream as a deterministic ~1.97s backward PTS
// jump at a GOP boundary (P-frame → IDR) in TestRTSPInterop_Matrix/gop60_720p30.
// The fix collapses same-timestamp keyframes into one group.
//
// This test casts a wide net (many groups) over the gop60_720p30 RTSP stream
// and asserts video PTS is monotonic (modulo a small jitter tolerance) in
// decode-arrival order. The matrix test's 3-group window rarely lands on the
// anomalous pair, so it can't reliably catch a regression; this one does.
//
// The vector disables B-frames (`-bf 0` + `-tune zerolatency`), so PTS == DTS
// and the decode-ordered series must not regress.
func TestRTSP_PTSMonotonic(t *testing.T) {
	if !ffpub.Available() {
		t.Skip("ffmpeg not on PATH")
	}

	mux := moqt.NewTrackMux(0)
	rtspAddr, serveURL := setupRTSPPipeline(t, mux)

	const path = "/live/ptsmono"
	pubCtx, cancelPub := context.WithCancel(context.Background())
	defer cancelPub()

	pub := ffpub.New(ffpub.Config{
		URL: fmt.Sprintf("rtsp://%s%s", rtspAddr, path),
		GOP: 60, Width: 1280, Height: 720, Framerate: 30,
	})
	if err := pub.Start(pubCtx); err != nil {
		t.Fatalf("publish: %v", err)
	}
	t.Cleanup(func() { _ = pub.Wait() })

	collectCtx, cancelCollect := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancelCollect()
	obs, err := (&Collector{
		Dialer:       &moqt.Dialer{TLSConfig: subscriberTLS(t)},
		URL:          serveURL,
		Path:         moqt.BroadcastPath(path),
		MaxGroups:    30,
		GroupTimeout: 4 * time.Second,
		WantTracks:   []string{"video"},
	}).Collect(collectCtx)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	cancelPub()

	vo := obs.Tracks["video"]
	if vo == nil {
		t.Fatalf("no video track; order=%v", obs.Order)
	}
	frames := vo.Frames
	t.Logf("collected %d video frames across %d groups", len(frames), vo.GroupCount)

	// A no-B-frame stream (-bf 0 + tune zerolatency) must be monotonic in decode
	// order. Allow a small jitter tolerance for encoder/clock quantization.
	const jitterUS = 50_000 // 50ms
	for i := 1; i < len(frames); i++ {
		if delta := frames[i].PTSUS - frames[i-1].PTSUS; delta < -jitterUS {
			start := i - 3
			if start < 0 {
				start = 0
			}
			end := i + 3
			if end > len(frames) {
				end = len(frames)
			}
			msg := fmt.Sprintf("PTS regression at frame %d: pts=%d -> %d (delta=%d us, %.3fs); not monotonic for a no-B-frame stream\n",
				i, frames[i-1].PTSUS, frames[i].PTSUS, delta, float64(-delta)/1e6)
			for j := start; j < end; j++ {
				d := int64(0)
				if j > 0 {
					d = frames[j].PTSUS - frames[j-1].PTSUS
				}
				mark := "  "
				if j == i {
					mark = ">>"
				}
				msg += fmt.Sprintf("  %s frame %d: pts=%-12d delta=%-10d key=%v\n",
					mark, j, frames[j].PTSUS, d, frames[j].IsKeyframe)
			}
			t.Fatal(msg)
		}
	}

	// Sanity: characterize the cadence so a silently-changed encoder output is noticed.
	var forwardDeltas []int64
	for i := 1; i < len(frames); i++ {
		if d := frames[i].PTSUS - frames[i-1].PTSUS; d > 0 {
			forwardDeltas = append(forwardDeltas, d)
		}
	}
	if med := medianInt64(forwardDeltas); med != 0 {
		t.Logf("median forward PTS delta: %d us (%.1f fps equivalent)", med, 1e6/float64(med))
	}
}

func medianInt64(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
	return xs[len(xs)/2]
}
