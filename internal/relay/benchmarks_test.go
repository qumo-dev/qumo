package relay

import (
	"fmt"
	"sync"
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
)

// ============================================================================
// Frame Pool Benchmarks
// ============================================================================

func BenchmarkFramePool_GetPut(b *testing.B) {
	pool := NewFramePool(DefaultNewFrameCapacity)

	b.ResetTimer()
	for range b.N {
		frame := pool.Get()
		frame.Write([]byte("test"))
		pool.Put(frame)
	}
}

func BenchmarkFramePool_GetPut_Parallel(b *testing.B) {
	pool := NewFramePool(DefaultNewFrameCapacity)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			frame := pool.Get()
			frame.Write([]byte("test"))
			pool.Put(frame)
		}
	})
}

func BenchmarkFramePool_LargeFrame(b *testing.B) {
	pool := NewFramePool(10 * 1024 * 1024) // 10MB frames

	b.ResetTimer()
	for range b.N {
		frame := pool.Get()
		frame.Write(make([]byte, 1024*1024))
		pool.Put(frame)
	}
}

// ============================================================================
// Group Cache Benchmarks
//
// BenchmarkGroupCache_ReadFanOut is the PRIMARY benchmark for this component:
// it models the real production access pattern (1 ingress writer, N subscriber
// readers fanning out). BenchmarkGroupCache_ConcurrentAppendAndNext below is
// SYNTHETIC — a relay never has 10 writers on one group — and is kept only to
// stress the append CAS path under contention.
// ============================================================================

// BenchmarkGroupCache_ReadFanOut measures N concurrent readers draining a
// pre-filled cache — the production read hot path (many subscribers per group).
// No writer, so no at-limit artifact; isolates how read cost scales with fan-out,
// which is the regime a relay is designed for. This is the benchmark that
// reflects real groupCache behavior; treat the multi-writer benchmarks as
// supplemental.
func BenchmarkGroupCache_ReadFanOut(b *testing.B) {
	frame := moqt.NewFrame(DefaultNewFrameCapacity)
	frame.Write([]byte("fanout"))
	for _, readers := range []int{1, 10, 50, 100, 200, 500} {
		b.Run(fmt.Sprintf("%dr", readers), func(b *testing.B) {
			gc := newGroupCache(1)
			pool := NewFramePool(DefaultNewFrameCapacity)
			for range 120 {
				gc.append(frame, pool)
			}
			b.ResetTimer()
			b.ReportAllocs()
			var wg sync.WaitGroup
			wg.Add(readers)
			for range readers {
				go func() {
					defer wg.Done()
					for i := 0; i < b.N; i++ {
						_ = gc.next(i % 120)
					}
				}()
			}
			wg.Wait()
		})
	}
}

func BenchmarkGroupCache_Next_HitRate(b *testing.B) {
	gc := newGroupCache(1)

	pool := NewFramePool(DefaultNewFrameCapacity)
	frame := moqt.NewFrame(DefaultNewFrameCapacity)
	frame.Write([]byte("test"))

	// Pre-populate with 100 frames
	for range 100 {
		gc.append(frame, pool)
	}

	b.ResetTimer()
	for range b.N {
		_ = gc.next(50) // Access middle frame
	}
}

// BenchmarkGroupCache_ConcurrentAppendAndNext is SYNTHETIC: it runs 10
// concurrent writers, which does not match the production model (a single
// ingress filler per group, appending frames in order). It is kept only to
// stress the append copy-on-write CAS path under contention. For the realistic
// 1-writer/N-reader access pattern, see BenchmarkGroupCache_ReadFanOut.
func BenchmarkGroupCache_ConcurrentAppendAndNext(b *testing.B) {
	const (
		writers   = 10
		readers   = 10
		framesCap = 1024 // bound the working set so the slice can't grow unboundedly
	)

	b.Run("10writers_10readers", func(b *testing.B) {
		pool := NewFramePool(DefaultNewFrameCapacity)
		gc := newGroupCache(1)
		frame := moqt.NewFrame(DefaultNewFrameCapacity)
		frame.Write([]byte("test data"))

		b.ResetTimer()
		b.ReportAllocs()

		// Work is scaled to b.N and divided across goroutines, so the reported
		// ns/op reflects one operation (previously each goroutine ran b.N
		// iterations, inflating the result by writers+readers).
		var wg sync.WaitGroup
		wg.Add(writers + readers)

		for range writers {
			go func() {
				defer wg.Done()
				for i := 0; i < b.N/writers; i++ {
					gc.append(frame, pool)
					// Reset periodically so the working set stays bounded at
					// high b.N (append otherwise self-limits at MaxFramesPerGroup).
					if i%framesCap == 0 {
						gc.resetForReuse()
					}
				}
			}()
		}

		for range readers {
			go func() {
				defer wg.Done()
				for i := 0; i < b.N/readers; i++ {
					_ = gc.next(i % 100)
				}
			}()
		}

		wg.Wait()
	})
}

// ============================================================================
// Group Ring Benchmarks
// ============================================================================

func BenchmarkGroupRing_Reserve(b *testing.B) {
	ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache := ring.reserve(moqt.GroupSequence(i))
		ring.decrRef(cache)
	}
}

// ============================================================================
// Group Ring Fill Benchmark
// ============================================================================

func BenchmarkGroupRing_Fill_VariableSize(b *testing.B) {
	tests := []struct {
		name       string
		frameCount int
		frameSize  int
	}{
		{"small_10frames_1KB", 10, 1024},
		{"medium_50frames_2KB", 50, 2048},
		{"large_100frames_10KB", 100, 10240},
	}

	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			pool := NewFramePool(DefaultNewFrameCapacity)
			ring := newGroupRing(DefaultGroupCacheSize, pool)

			frame := moqt.NewFrame(tt.frameSize)
			frame.Write(make([]byte, tt.frameSize))

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				cache := ring.reserve(moqt.GroupSequence(i))
				for j := 0; j < tt.frameCount; j++ {
					cache.append(frame, pool)
				}
				ring.decrRef(cache)
			}
		})
	}
}

// ============================================================================
// Broadcast Operation Benchmarks
// ============================================================================

// func BenchmarkTrackDistributor_Broadcast removed: subscribe/unsubscribe API replaced by broadcastNotify

// BenchmarkTrackDistributor_Broadcast measures dist.broadcast() — the new-data
// fan-out call — swept across subscriber counts. It replaces the pre-#332 O(N)
// benchmark that sent on a per-subscriber channel slice under an RWMutex.
// broadcast() is now a single broadcastNotify.notify() (atomic seq bump +
// close-and-recreate), so ns/op must stay FLAT across the sweep — that flatness
// is the invariant being guarded: if a future change re-introduces per-
// subscriber work, ns/op will scale with N and benchstat will flag it. There
// are no live waiter goroutines by design: close() is non-blocking and the wake
// is async, so waiters would only add scheduler noise without changing the
// measured call (the old benchmark made the same choice).
func BenchmarkTrackDistributor_Broadcast(b *testing.B) {
	for _, n := range []int{1, 10, 100, 1000} {
		b.Run(fmt.Sprintf("%d_subscribers", n), func(b *testing.B) {
			dist := &trackDistributor{}
			dist.notify.init()
			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				dist.broadcast()
			}
		})
	}
}

// BenchmarkBroadcastNotify_Listen measures the cost of a single listen() — the
// read-side primitive every egress goroutine calls on each wakeup (and that the
// deliverGroup seq-guard adds one extra call of per trickle wakeup). It is a
// pure atomic.Pointer load returning a notifyState value, so this is the floor
// cost of participating in the broadcast. A regression here scales directly
// into fan-out: it is paid by every subscriber, every wakeup.
func BenchmarkBroadcastNotify_Listen(b *testing.B) {
	var n broadcastNotify
	n.init()
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		_ = n.listen()
	}
}

// BenchmarkBroadcastNotify_Listen_Parallel measures listen() under the relay's
// actual access pattern: many readers (one egress goroutine per subscriber)
// reading concurrently with a single writer calling notify(). The writer's
// Store invalidates the cache line the readers Load, modeling the real fan-out
// cost; a regression here flags growing read-side contention. The notifier runs
// hot, so its allocations (a channel + state per notify) appear in the harness
// but not in the measured ns/op, which is the reader-side load only.
func BenchmarkBroadcastNotify_Listen_Parallel(b *testing.B) {
	var n broadcastNotify
	n.init()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				n.notify()
			}
		}
	})

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s := n.listen()
			_ = s.seq
		}
	})

	b.StopTimer()
	close(stop)
	wg.Wait()
}

// BenchmarkBroadcastNotify_Notify measures the write-side primitive — the cost
// of one broadcast: an atomic seq bump plus a close-and-recreate channel swap
// under the serialize mutex. This is what every delivered group pays (once per
// group, single-writer) and it allocates a fresh channel + notifyState each
// call — allocations inherent to the wake-all design, so this bench guards both
// the cost and the alloc count.
func BenchmarkBroadcastNotify_Notify(b *testing.B) {
	var n broadcastNotify
	n.init()
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		n.notify()
	}
}

// ============================================================================
// Subscribe/Unsubscribe Benchmarks
// ============================================================================

// func BenchmarkTrackDistributor_SubscribeUnsubscribe removed: subscribe/unsubscribe API replaced by broadcastNotify

// func BenchmarkTrackDistributor_SubscribeUnsubscribe_Parallel removed: subscribe/unsubscribe API replaced by broadcastNotify

// ============================================================================
// Memory Allocation Tracking Benchmarks
// ============================================================================

func BenchmarkMemAllocs_FramePool(b *testing.B) {
	pool := NewFramePool(DefaultNewFrameCapacity)

	b.ResetTimer()
	b.ReportAllocs()

	for range b.N {
		frame := pool.Get()
		pool.Put(frame)
	}
}

func BenchmarkMemAllocs_GroupCache_Append(b *testing.B) {
	const framesCap = 4096

	pool := NewFramePool(DefaultNewFrameCapacity)
	gc := newGroupCache(1)

	frame := moqt.NewFrame(DefaultNewFrameCapacity)
	frame.Write([]byte("test"))

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		gc.append(frame, pool)
		// groupCache.append grows frames without bound; trim periodically to
		// bound memory without distorting the amortized allocs/op measurement.
		if i%framesCap == 0 {
			gc.resetForReuse()
		}
	}
}

func BenchmarkMemAllocs_GroupRing_Reserve(b *testing.B) {
	ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		cache := ring.reserve(moqt.GroupSequence(i))
		ring.decrRef(cache)
	}
}

// ============================================================================
// Contention / Lock Pressure Benchmarks
// ============================================================================

func BenchmarkLockPressure_GroupCache(b *testing.B) {
	const framesCap = 512 // bound the working set so the slice can't grow unboundedly

	tests := []struct {
		name    string
		writers int
		readers int
	}{
		{"1w_1r", 1, 1},
		{"1w_10r", 1, 10},
		{"10w_10r", 10, 10},
		{"1w_100r", 1, 100},
	}

	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			pool := NewFramePool(DefaultNewFrameCapacity)
			gc := newGroupCache(1)

			frame := moqt.NewFrame(DefaultNewFrameCapacity)
			frame.Write([]byte("test"))

			b.ResetTimer()
			b.ReportAllocs()

			var wg sync.WaitGroup

			// Writers: work scales with b.N (previously a fixed iteration
			// count made ns/op meaningless and ignored b.N).
			for w := 0; w < tt.writers; w++ {
				wg.Go(func() {
					for i := 0; i < b.N/tt.writers; i++ {
						gc.append(frame, pool)
						// groupCache.append grows frames without bound; trim
						// periodically (under the same lock append uses) to
						// keep memory bounded at high b.N.
						if i%framesCap == 0 {
							gc.resetForReuse()
						}
					}
				})
			}

			// Readers: work scales with b.N, divided across readers.
			for r := 0; r < tt.readers; r++ {
				wg.Go(func() {
					for i := 0; i < b.N/tt.readers; i++ {
						_ = gc.next(i % 10)
					}
				})
			}

			wg.Wait()
		})
	}
}

// func BenchmarkLockPressure_TrackDistributor_Subscribe removed: subscribe/unsubscribe API replaced by broadcastNotify

// ============================================================================
// Ring Contention Benchmarks
// ============================================================================

func BenchmarkGroupRing_ConcurrentReserveAndGet(b *testing.B) {
	tests := []struct {
		name    string
		writers int
		readers int
	}{
		{"1w_1r", 1, 1},
		{"1w_10r", 1, 10},
		{"10w_10r", 10, 10},
	}

	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)

			b.ResetTimer()
			b.ReportAllocs()

			var wg sync.WaitGroup

			// Writers
			for w := 0; w < tt.writers; w++ {
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					for i := 0; i < b.N/tt.writers; i++ {
						cache := ring.reserve(moqt.GroupSequence(i + id*1000000))
						ring.decrRef(cache)
					}
				}(w)
			}

			// Readers
			for r := 0; r < tt.readers; r++ {
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					for i := 0; i < b.N/tt.readers; i++ {
						cache := ring.get(ringPosition(i % ring.size))
						if cache != nil {
							ring.decrRef(cache)
						}
					}
				}(r)
			}

			wg.Wait()
		})
	}
}
