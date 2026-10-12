package relay

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// groupCache tests
// ============================================================================

// collectFrames returns all frames currently in the group, for test assertions.
// It is a free function, not a method on groupCache, so test-only scaffolding
// does not leak into the production type's API. It reads through the production
// path (next), so tests exercise the same read code production uses; a
// reserved-but-not-yet-stored slot reads back as nil.
func collectFrames(gc *groupCache) []*moqt.Frame {
	n := int(gc.count.Load())
	out := make([]*moqt.Frame, n)
	for i := range n {
		out[i] = gc.next(i)
	}
	return out
}

func TestGroupCache_Append(t *testing.T) {
	gc := newGroupCache(1)

	frame := moqt.NewFrame(100)
	frame.Write([]byte("test data"))
	gc.append(frame, DefaultFramePool)

	require.Len(t, collectFrames(gc), 1)
	assert.NotSame(t, frame, collectFrames(gc)[0], "frame should be cloned")
}

func TestGroupCache_Append_ClonesData(t *testing.T) {
	gc := newGroupCache(1)

	original := moqt.NewFrame(100)
	original.Write([]byte("original"))
	gc.append(original, DefaultFramePool)

	original.Reset()
	original.Write([]byte("modified"))

	cached := collectFrames(gc)[0]
	assert.NotEqual(t, 0, cached.Len(), "cached frame should retain original data")
}

func TestGroupCache_Append_Multiple(t *testing.T) {
	gc := newGroupCache(1)

	for range 5 {
		f := moqt.NewFrame(100)
		f.Write([]byte("data"))
		gc.append(f, DefaultFramePool)
	}

	assert.Len(t, collectFrames(gc), 5)
}

func TestGroupCache_Next(t *testing.T) {
	gc := newGroupCache(1)
	for range 3 {
		frame := moqt.NewFrame(100)
		gc.append(frame, DefaultFramePool)
	}

	tests := map[string]struct {
		idx     int
		wantNil bool
	}{
		"index 0":       {idx: 0, wantNil: false},
		"index 1":       {idx: 1, wantNil: false},
		"index 2":       {idx: 2, wantNil: false},
		"negative":      {idx: -1, wantNil: true},
		"out of bounds": {idx: 3, wantNil: true},
		"large":         {idx: 100, wantNil: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := gc.next(tt.idx)
			if tt.wantNil {
				assert.Nil(t, got)
			} else {
				assert.NotNil(t, got)
			}
		})
	}
}

func TestGroupCache_ConcurrentAppend(t *testing.T) {
	gc := newGroupCache(1)

	const goroutines = 10
	const appendsPerGoroutine = 20 // Stay under MaxFramesPerGroup (256)

	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range appendsPerGoroutine {
				frame := moqt.NewFrame(100)
				frame.Write([]byte("data"))
				gc.append(frame, DefaultFramePool)
			}
		})
	}
	wg.Wait()

	assert.Len(t, collectFrames(gc), goroutines*appendsPerGoroutine)
}

func TestGroupCache_IsComplete(t *testing.T) {
	gc := newGroupCache(1)

	assert.False(t, gc.isComplete(), "should not be complete before markComplete")
	gc.markComplete()
	assert.True(t, gc.isComplete())
}

// ============================================================================
// groupRing tests
// ============================================================================

func TestGroupRing_Head_Initial(t *testing.T) {
	ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)
	assert.Equal(t, ringPosition(0), ring.head())
}

func TestGroupRing_Head_AfterPositionSet(t *testing.T) {
	ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)
	ring.pos.Store(10)
	assert.Equal(t, ringPosition(10), ring.head())
}

func TestGroupRing_EarliestAvailable(t *testing.T) {
	ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)
	size := ringPosition(ring.size)

	tests := map[string]struct {
		head     ringPosition
		expected ringPosition
	}{
		"zero":            {head: 0, expected: 1},
		"one":             {head: 1, expected: 1},
		"five":            {head: 5, expected: 1},
		"exactly at size": {head: size, expected: 1},
		"one past size":   {head: size + 1, expected: 2},
		"five past size":  {head: size + 5, expected: 6},
		"large value":     {head: 100, expected: 100 - size + 1},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ring.pos.Store(uint64(tt.head))
			assert.Equal(t, tt.expected, ring.earliestAvailable(), "head=%d", tt.head)
		})
	}
}

func TestGroupRing_EarliestAvailable_AtBoundary(t *testing.T) {
	ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)

	ring.pos.Store(uint64(ring.size))
	assert.Equal(t, ringPosition(1), ring.earliestAvailable())

	ring.pos.Store(uint64(ring.size + 1))
	assert.Equal(t, ringPosition(2), ring.earliestAvailable())
}

func TestGroupRing_Get(t *testing.T) {
	ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)

	testCache := newGroupCache(5)
	testCache.pos = 5
	idx := uint64(5) % uint64(ring.size)
	ring.caches[idx].Store(testCache)

	got := ring.get(5)
	require.NotNil(t, got)
	assert.Equal(t, moqt.GroupSequence(5), got.seq)
}

// A publisher numbers its groups as it likes: a track joined late starts past
// 1, and one that dropped groups has gaps. The ring hands groups out by the
// order they arrived in, each keeping its own sequence.
func TestGroupRing_Get_SequencesNotStartingAtOneOrDense(t *testing.T) {
	tests := map[string][]moqt.GroupSequence{
		"starting past one": {3, 4, 5},
		"with gaps":         {1, 5, 9},
		"a single late one": {7},
		"wrapping the ring": {10, 20, 30, 40, 50, 60},
	}
	for name, seqs := range tests {
		t.Run(name, func(t *testing.T) {
			const size = 4
			ring := newGroupRing(size, DefaultFramePool)

			for _, seq := range seqs {
				ring.reserve(seq)
			}

			require.Equal(t, ringPosition(len(seqs)), ring.head())
			for pos := ring.earliestAvailable(); pos <= ring.head(); pos++ {
				got := ring.get(pos)
				require.NotNil(t, got, "position %d", pos)
				assert.Equal(t, seqs[pos-1], got.seq, "position %d", pos)
			}
		})
	}
}

func TestGroupRing_Get_BeforeAnyAdd(t *testing.T) {
	ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)
	// Should not panic; may return nil or an old slot
	_ = ring.get(1)
}

func TestGroupRing_Capacity(t *testing.T) {
	tests := map[string]struct {
		size int
	}{
		"default": {size: DefaultGroupCacheSize},
		"small":   {size: 4},
		"large":   {size: 64},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ring := newGroupRing(tt.size, DefaultFramePool)
			assert.Equal(t, tt.size, ring.size)
			assert.Len(t, ring.caches, tt.size)
		})
	}
}

func TestGroupRing_ConcurrentAccess(t *testing.T) {
	ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)

	done := make(chan bool, 20)
	for i := range 10 {
		go func(id int) {
			for j := range 100 {
				cache := newGroupCache(moqt.GroupSequence(id*100 + j))
				idx := (id*100 + j) % ring.size
				ring.caches[idx].Store(cache)
				ring.pos.Add(1)
			}
			done <- true
		}(i)
	}
	for range 10 {
		go func() {
			for j := range 100 {
				_ = ring.head()
				_ = ring.earliestAvailable()
				_ = ring.get(ringPosition(j))
			}
			done <- true
		}()
	}
	for range 20 {
		<-done
	}
}

// ============================================================================
// groupRing.reserve tests
// ============================================================================

func TestGroupRing_Reserve_SlotAllocated(t *testing.T) {
	ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)

	cache := ring.reserve(moqt.GroupSequence(1))

	require.NotNil(t, cache)
	assert.Equal(t, moqt.GroupSequence(1), cache.seq)
	assert.Equal(t, ringPosition(1), ring.head())
}

func TestGroupRing_Reserve_AdvancesHead(t *testing.T) {
	ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)

	for i := range 5 {
		ring.reserve(moqt.GroupSequence(i + 1))
	}

	assert.Equal(t, ringPosition(5), ring.head())
}

func TestGroupRing_Reserve_SlotVisibleBeforeFill(t *testing.T) {
	ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)

	// reserve increments pos and stores at slot (pos % size).
	// get(seq) looks up slot (seq % size).
	// Both align when seq == post-increment pos (normal sequential arrival).
	cache := ring.reserve(moqt.GroupSequence(1))

	got := ring.get(ringPosition(1))
	require.NotNil(t, got)
	assert.Same(t, cache, got)
	assert.False(t, cache.isComplete(), "should not be complete before fill")
}

func TestGroupRing_Reserve_WrapAround(t *testing.T) {
	ring := newGroupRing(4, DefaultFramePool)

	for i := range 8 {
		ring.reserve(moqt.GroupSequence(i + 1))
	}

	assert.Equal(t, ringPosition(8), ring.head())
	// seq=5 overwrote seq=1 at the same slot (mod 4).
	got := ring.get(ringPosition(5))
	require.NotNil(t, got)
	assert.Equal(t, moqt.GroupSequence(5), got.seq)
}

// ============================================================================
// groupRing.fill tests
// ============================================================================

func TestGroupRing_Fill_FramesStored(t *testing.T) {
	ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)
	cache := ring.reserve(moqt.GroupSequence(1))

	src := &fakeFrameSource{
		frames: [][]byte{[]byte("frame-0"), []byte("frame-1"), []byte("frame-2")},
	}

	ring.fill(src, cache, nil)

	require.True(t, cache.isComplete())
	for i := range 3 {
		assert.NotNil(t, cache.next(i), "frame %d should exist", i)
	}
	assert.Nil(t, cache.next(3), "no frame beyond index 2")
}

func TestGroupRing_Fill_MarksComplete(t *testing.T) {
	ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)
	cache := ring.reserve(moqt.GroupSequence(1))
	src := &fakeFrameSource{frames: [][]byte{[]byte("x")}}

	assert.False(t, cache.isComplete())
	ring.fill(src, cache, nil)
	assert.True(t, cache.isComplete())
}

func TestGroupRing_Fill_NotifyCount(t *testing.T) {
	tests := map[string]struct {
		frameCount    int
		expectedCalls int32
	}{
		"zero frames": {frameCount: 0, expectedCalls: 1}, // completion only
		"one frame":   {frameCount: 1, expectedCalls: 2}, // 1 frame + completion
		"five frames": {frameCount: 5, expectedCalls: 6},
		"ten frames":  {frameCount: 10, expectedCalls: 11},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)
			cache := ring.reserve(moqt.GroupSequence(1))

			frames := make([][]byte, tt.frameCount)
			for i := range tt.frameCount {
				frames[i] = []byte("data")
			}
			src := &fakeFrameSource{frames: frames}

			var calls atomic.Int32
			ring.fill(src, cache, func(int) { calls.Add(1) })

			assert.Equal(t, tt.expectedCalls, calls.Load())
		})
	}
}

func TestGroupRing_Fill_ClonesFrames(t *testing.T) {
	ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)
	cache := ring.reserve(moqt.GroupSequence(1))
	payload := []byte("original")
	src := &fakeFrameSource{frames: [][]byte{payload}}

	ring.fill(src, cache, nil)

	stored := cache.next(0)
	require.NotNil(t, stored)
	payload[0] = 'X'
	assert.Equal(t, "original", string(stored.Body()), "stored frame must be a clone")
}

func TestGroupRing_Fill_NilOnFrame(t *testing.T) {
	ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)
	cache := ring.reserve(moqt.GroupSequence(1))
	src := &fakeFrameSource{frames: [][]byte{[]byte("data")}}

	// Must not panic when onFrame is nil.
	assert.NotPanics(t, func() {
		ring.fill(src, cache, nil)
	})
	assert.True(t, cache.isComplete())
}

// ============================================================================
// groupRing.fill concurrent tests
// ============================================================================

func TestGroupRing_Fill_ConcurrentGroups(t *testing.T) {
	const numGroups = 16
	const framesPerGroup = 8
	ring := newGroupRing(numGroups, DefaultFramePool)

	caches := make([]*groupCache, numGroups)
	for i := range numGroups {
		caches[i] = ring.reserve(moqt.GroupSequence(i + 1))
	}

	var wg sync.WaitGroup
	for i := range numGroups {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			frames := make([][]byte, framesPerGroup)
			for j := range framesPerGroup {
				frames[j] = fmt.Appendf(nil, "g%d-f%d", idx, j)
			}
			ring.fill(&fakeFrameSource{frames: frames}, caches[idx], nil)
		}(i)
	}
	wg.Wait()

	for i, c := range caches {
		assert.True(t, c.isComplete(), "group %d should be complete", i)
		for j := range framesPerGroup {
			assert.NotNil(t, c.next(j), "group %d frame %d should exist", i, j)
		}
	}
}

func TestGroupRing_Fill_ConcurrentNotifyCount(t *testing.T) {
	const numGroups = 8
	const framesPerGroup = 4
	ring := newGroupRing(numGroups, DefaultFramePool)

	caches := make([]*groupCache, numGroups)
	for i := range numGroups {
		caches[i] = ring.reserve(moqt.GroupSequence(i + 1))
	}

	var total atomic.Int64
	notify := func(int) { total.Add(1) }

	var wg sync.WaitGroup
	for i := range numGroups {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			frames := make([][]byte, framesPerGroup)
			for j := range framesPerGroup {
				frames[j] = []byte("x")
			}
			ring.fill(&fakeFrameSource{frames: frames}, caches[idx], notify)
		}(i)
	}
	wg.Wait()

	expected := int64(numGroups * (framesPerGroup + 1))
	assert.Equal(t, expected, total.Load())
}

// TestGroupRing_Fill_ConcurrentFasterThanSerial verifies that concurrent fills
// complete in roughly framesPerGroup×delay time, not numGroups×framesPerGroup×delay.
// Uses testing/synctest for deterministic time control.
func TestGroupRing_Fill_ConcurrentFasterThanSerial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const numGroups = 6
		const framesPerGroup = 3
		const delay = 10 * time.Millisecond

		ring := newGroupRing(numGroups, DefaultFramePool)
		caches := make([]*groupCache, numGroups)
		for i := range numGroups {
			caches[i] = ring.reserve(moqt.GroupSequence(i + 1))
		}

		allDone := make(chan struct{})
		var wg sync.WaitGroup
		for i := range numGroups {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				frames := make([][]byte, framesPerGroup)
				for j := range framesPerGroup {
					frames[j] = []byte("x")
				}
				src := &slowFrameSource{
					frames: frames,
					delay:  delay,
				}
				ring.fill(src, caches[idx], nil)
			}(i)
		}
		go func() {
			wg.Wait()
			close(allDone)
		}()

		// All goroutines sleep concurrently; advance past framesPerGroup×delay.
		serialTime := time.Duration(numGroups*framesPerGroup) * delay
		concurrentTime := time.Duration(framesPerGroup) * delay

		time.Sleep(concurrentTime + delay)
		synctest.Wait()

		select {
		case <-allDone:
			// good: completed in roughly framesPerGroup×delay
		default:
			t.Fatalf("fills should complete within %v, not the serial %v",
				concurrentTime+delay, serialTime)
		}
	})
}

// ============================================================================
// WaitGroup / done-channel ordering test
// ============================================================================

// TestGroupRing_Fill_WaitGroupBlocksDone verifies that a WaitGroup tracking fill
// goroutines prevents the done channel from being closed until all fills
// complete — this models the LIFO defer ordering in trackDistributor.ingest.
func TestGroupRing_Fill_WaitGroupBlocksDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)
		cache := ring.reserve(moqt.GroupSequence(1))

		frames := [][]byte{[]byte("a"), []byte("b"), []byte("c")}
		src := &slowFrameSource{
			frames: frames,
			delay:  10 * time.Millisecond,
		}

		done := make(chan struct{})
		fillDone := make(chan struct{})

		var wg sync.WaitGroup
		wg.Go(func() {
			ring.fill(src, cache, nil)
			close(fillDone)
		})
		go func() {
			wg.Wait()
			close(done)
		}()

		synctest.Wait()

		// Advance time past all per-frame sleeps (3 × 10 ms).
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()

		select {
		case <-fillDone:
		default:
			t.Fatal("fill should have completed after 50ms")
		}
		select {
		case <-done:
		default:
			t.Fatal("done should be closed after fill completed")
		}
	})
}

// ============================================================================
// Benchmarks
// ============================================================================

func BenchmarkGroupCache_Append(b *testing.B) {
	gc := newGroupCache(1)
	frame := moqt.NewFrame(1500)
	frame.Write(make([]byte, 1000))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		gc.append(frame, DefaultFramePool)
	}
}

func BenchmarkGroupCache_Next(b *testing.B) {
	gc := newGroupCache(1)
	for range 100 {
		frame := moqt.NewFrame(1500)
		gc.append(frame, DefaultFramePool)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = gc.next(i % 100)
	}
}

func BenchmarkGroupRing_Get(b *testing.B) {
	ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)
	for i := range ring.size {
		cache := newGroupCache(moqt.GroupSequence(i))
		ring.caches[i].Store(cache)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ring.get(ringPosition(i % ring.size))
	}
}

func BenchmarkGroupRing_Ingest(b *testing.B) {
	pool := NewFramePool(DefaultNewFrameCapacity)
	ring := newGroupRing(DefaultGroupCacheSize, pool)

	frame := moqt.NewFrame(DefaultNewFrameCapacity)
	frame.Write(make([]byte, 1000))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache := ring.reserve(moqt.GroupSequence(i))
		for range 10 {
			cache.append(frame, pool)
		}
		ring.decrRef(cache)
	}
}

func TestGroupRing_Stress(t *testing.T) {
	const numReaders = 10
	const numGroups = 1000
	const ringSize = 8

	pool := NewFramePool(DefaultNewFrameCapacity)
	ring := newGroupRing(ringSize, pool)

	var wg sync.WaitGroup

	// Start readers
	for r := range numReaders {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			for g := 1; g < numGroups; g++ {
				// Sleep to simulate processing lag
				time.Sleep(time.Duration(g%3) * time.Microsecond)

				cache := ring.get(ringPosition(g))
				if cache != nil {
					// Read frame
					_ = cache.next(0)
					ring.decrRef(cache)
				}
			}
		}(r)
	}

	// Start writer (ingest)
	wg.Go(func() {
		for g := 1; g < numGroups; g++ {
			cache := ring.reserve(moqt.GroupSequence(g))

			// Simulate concurrent fill
			wg.Add(1)
			go func(c *groupCache, groupSeq int) {
				defer wg.Done()
				time.Sleep(time.Duration(groupSeq%5) * time.Microsecond)

				src := &fakeFrameSource{
					frames: [][]byte{[]byte("frame-data")},
				}
				ring.fill(src, c, nil)
			}(cache, g)
		}
	})

	wg.Wait()
}
