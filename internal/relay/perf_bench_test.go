package relay

import (
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
)

// BenchmarkNewUUIDv4 targets the per-session broadcast ID generation in the
// meter (stdlib uuid.NewV4).
func BenchmarkNewUUIDv4(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = newUUIDv4()
	}
}

// BenchmarkGroupRing_Fill targets the ingest fill path (groupRing.fill),
// including the per-fill cleanup of the pool buffer and cache reference that
// the defer-elimination change moves to an explicit tail call.
func BenchmarkGroupRing_Fill(b *testing.B) {
	pool := NewFramePool(DefaultNewFrameCapacity)
	ring := newGroupRing(DefaultGroupCacheSize, pool)

	frames := make([][]byte, 8)
	for i := range frames {
		frames[i] = make([]byte, 1024)
	}
	src := &fakeFrameSource{frames: frames}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache := ring.reserve(moqt.GroupSequence(i + 1))
		ring.fill(src, cache, nil)
	}
}
