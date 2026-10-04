package relay

import (
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
)

// BenchmarkNext_Serial compares serial next() calls with baseline vs lockless
func BenchmarkNext_Serial(b *testing.B) {
	gc := newGroupCache(1)

	pool := NewFramePool(DefaultNewFrameCapacity)
	frame := moqt.NewFrame(DefaultNewFrameCapacity)
	frame.Write([]byte("test"))

	// Pre-populate with 100 frames
	for range 100 {
		gc.append(frame, pool)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = gc.next(i % 100)
	}
}
