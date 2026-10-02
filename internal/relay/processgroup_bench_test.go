package relay

import (
	"context"
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
)

// BenchmarkProcessGroup exercises the ingest per-group path: ring.reserve +
// dispatch to the fill worker pool. Reports allocs/op so the per-group dispatch
// cost is visible. fakeFrameSource makes fill synchronous and fast, isolating
// the per-group dispatch overhead from the frame work.
func BenchmarkProcessGroup(b *testing.B) {
	dist := newTrackDistributor(newTrackManager(0, nil), "bench/processgroup", nil)

	src := &fakeFrameSource{frames: [][]byte{make([]byte, 1024)}}
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dist.processGroup(ctx, moqt.GroupSequence(i+1), src)
	}
	b.StopTimer()
	// Drain the worker pool so in-flight fills complete before the run ends.
	close(dist.fillJobs)
	dist.fillWg.Wait()
}
