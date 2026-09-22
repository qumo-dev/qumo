package ingest

import (
	"io"
	"log/slog"
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
)

// BenchmarkSession_PushVideo measures the real ingest per-frame cost through
// the public Session API (RegisterVideo + PushVideo): frame envelope encode,
// group boundary handling, append, and the fan-out notify — the complete path
// a publisher's frame takes before MoQT egress picks it up. It is written
// against the stable public API so the same file runs on base and head for a
// fair benchstat comparison; a regression here is a regression in production
// ingest, regardless of what the micro-benchmarks say.
//
// No subscribers are attached: the design isolates the writer-side path. The
// fan-out dimension is covered by BenchmarkTrackBufferNotify_Fanout.
func BenchmarkSession_PushVideo(b *testing.B) {
	// NewSession/Close log via slog's default handler on every invocation of
	// the benchmark body; those lines land on stdout in the middle of the
	// result line and corrupt both local and CI benchstat parsing. Silence
	// the default logger for the duration and restore it after.
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prev)

	mux := moqt.NewTrackMux(0)
	sess, err := NewSession(mux, "/live/bench")
	if err != nil {
		b.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()
	if err := sess.RegisterVideo(&AVCConfig{
		NALULenSize: 4,
		SPS:         [][]byte{{0x67, 0x64, 0x00, 0x1F}},
		PPS:         [][]byte{{0x68, 0xEB}},
	}); err != nil {
		b.Fatalf("RegisterVideo: %v", err)
	}

	data := make([]byte, 1024) // ~1kB Annex-B frame
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		// Keyframe every 30 frames to open a new group (GOP) periodically.
		sess.PushVideo(int64(i)*33333, data, i%30 == 0)
	}
}
