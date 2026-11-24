//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/require"
)

// TestRTSPPlayback_FrameIntegrity is the regression guard for the RTSP
// "broken picture + audio pops" defects. Both stemmed from RTSP ingest emitting
// the wrong MoQT group/frame shape for multi-NALU video access units and
// multi-AU audio packets — defects the interop gate does not catch because the
// gate checks codec strings and frame counts but never decodes or orders media.
//
// Video: ffmpeg's RTSP muxer emits several IDR NALUs at the same RTP timestamp
// within one access unit. Ingest used to push one MoQT frame per NALU, so a
// keyframe produced several same-PTS frames in one group; the player marks only
// the first as `key` and the rest as `delta`, corrupting the decode. The fix
// aggregates same-timestamp NALUs into one AVCC sample per access unit, so every
// video frame must now have a unique PTS.
//
// Audio: an mpeg4-generic RTP packet packs 3–4 AAC frames. Ingest used to push
// each as its own MoQT group; a packet thus burst N concurrent QUIC streams
// (MoQT = one group per stream) that gomoqt delivers in stream-arrival order,
// so the subscriber received AAC frames out of PTS order → pops. The fix
// coalesces one packet's AUs into a single group, so audio PTS must now be
// strictly monotonic in arrival order.
//
// Run with: go test -tags=integration ./internal/integration/...
func TestRTSPPlayback_FrameIntegrity(t *testing.T) {
	if !ffpubAvailable() {
		t.Skip("ffmpeg not on PATH")
	}
	mux := moqt.NewTrackMux(0)
	rtspAddr, serveURL := setupRTSPPipeline(t, mux)

	const path = "/live/playback"
	pubCtx, cancelPub := context.WithCancel(context.Background())
	defer cancelPub()
	pub := newFFPub(ffpubConfig{
		URL:   fmt.Sprintf("rtsp://%s%s", rtspAddr, path),
		Audio: true,
		GOP:   30, Width: 320, Height: 240, Framerate: 30,
	})
	require.NoError(t, pub.Start(pubCtx))
	t.Cleanup(func() { _ = pub.Wait() })
	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := (&moqt.Dialer{TLSConfig: subscriberTLS(t)}).Dial(ctx, serveURL, moqt.NewTrackMux(0))
	require.NoError(t, err)
	defer sess.CloseWithError(moqt.NoError, "done")

	// Drain the catalog so the media tracks resolve.
	ctr, err := sess.Subscribe(ctx, moqt.BroadcastPath(path), "catalog", nil)
	require.NoError(t, err)
	cgr, err := ctr.AcceptGroup(ctx)
	require.NoError(t, err)
	cbuf := moqt.NewFrame(4096)
	for range cgr.Frames(cbuf) {
	}

	vtr, err := sess.Subscribe(ctx, moqt.BroadcastPath(path), "video", nil)
	require.NoError(t, err)
	atr, err := sess.Subscribe(ctx, moqt.BroadcastPath(path), "audio", nil)
	require.NoError(t, err)

	// --- Video: collect a few groups. ---
	type vframe struct{ pts int64 }
	var vframes []vframe
	vbuf := moqt.NewFrame(1 << 20)
	for groups := 0; groups < 3; groups++ {
		gctx, gcancel := context.WithTimeout(ctx, 4*time.Second)
		gr, err := vtr.AcceptGroup(gctx)
		gcancel()
		if err != nil {
			break
		}
		for f := range gr.Frames(vbuf) {
			pts, _, derr := decodeMediaFrame(f.Body())
			if derr != nil {
				continue
			}
			vframes = append(vframes, vframe{pts: pts})
		}
	}

	// --- Audio: collect concurrently while the publisher is still live. ---
	type aframe struct{ pts int64 }
	var aframes []aframe
	abuf := moqt.NewFrame(1 << 16)
	for i := 0; i < 20; i++ {
		gctx, gcancel := context.WithTimeout(ctx, 2*time.Second)
		gr, err := atr.AcceptGroup(gctx)
		gcancel()
		if err != nil {
			break
		}
		for f := range gr.Frames(abuf) {
			pts, _, derr := decodeMediaFrame(f.Body())
			if derr != nil {
				continue
			}
			aframes = append(aframes, aframe{pts: pts})
		}
	}
	cancelPub()

	// Video invariant: every access unit is one frame, so no two frames share a
	// PTS. (B-frame composition reorder can still make PTS non-monotonic in
	// decode order, but this vector disables B-frames; either way, distinct AUs
	// must have distinct PTS.)
	require.GreaterOrEqual(t, len(vframes), 10, "need a handful of video frames")
	seenPTS := make(map[int64]struct{}, len(vframes))
	for i, f := range vframes {
		if _, dup := seenPTS[f.pts]; dup {
			t.Fatalf("video frame %d duplicates PTS %d — same-AU NALUs were not aggregated into one frame", i, f.pts)
		}
		seenPTS[f.pts] = struct{}{}
	}

	// Audio invariant: AAC frames arrive in PTS order. A regression that bursts
	// groups makes gomoqt deliver them out of order (observed as a zigzag).
	require.GreaterOrEqual(t, len(aframes), 10, "need a handful of audio frames")
	for i := 1; i < len(aframes); i++ {
		delta := aframes[i].pts - aframes[i-1].pts
		if delta <= 0 {
			t.Fatalf("audio PTS regressed at frame %d: %d -> %d (delta=%d) — multi-AU packet bursted into groups delivered out of order",
				i, aframes[i-1].pts, aframes[i].pts, delta)
		}
		// One AAC-LC frame at 48 kHz is ~21.3 ms; a coalesced group still steps
		// by whole frames, so flag anything beyond a generous bound.
		if delta > 100_000 {
			t.Fatalf("audio PTS gap at frame %d: delta=%d us exceeds 100 ms — frames lost or mis-timestamped", i, delta)
		}
	}
}
