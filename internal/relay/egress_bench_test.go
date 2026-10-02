package relay

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
)

// fanoutSizes is the subscriber-count sweep used to characterize how the
// per-frame egress accounting cost scales with fan-out. 1 → 100 covers the
// single-subscriber case through the relay's realistic operating regime.
var fanoutSizes = []int{1, 2, 4, 16, 64, 100}

// runEgressFanout drives `nSubs` goroutines that each repeat the per-frame
// egress-side accounting the relay does on every delivered frame:
//
//	frame := cache.next(i)
//	n := frame.Len()
//	d.egressCounter.Add(float64(n))   // ONE prometheus.Counter per track, shared
//
// withCounters=false drops the shared-counter Add calls, yielding the
// contention-free baseline (next + Len only). Comparing the two across the
// fan-out sweep isolates the cache-line ping-pong cost of the shared counters.
//
// batched=true models the deliverGroup optimization (#333): instead of one
// egressCounter.Add per frame, bytes are accumulated locally and flushed once
// per group (every nFrames frames), cutting shared-counter CAS operations from
// O(frames) to O(groups). The per-frame (batched=false) and
// per-group (batched=true) sweeps together let benchstat show the batching win
// directly under fan-out contention.
//
// This is a maximal-contention upper bound: in production gw.WriteFrame (a
// QUIC stream write) spaces the Add calls apart in time, reducing contention.
// If the counters are NOT a bottleneck under maximal contention, they are
// definitively not one in production. The group is pre-filled and complete, so
// there is no waiting/wakeup on this path — next() always hits.
func runEgressFanout(b *testing.B, nSubs int, withCounters, batched bool) {
	b.Helper()

	trackID := fmt.Sprintf("bench-egress-%d-%d", nSubs, time.Now().UnixNano())
	dist := newTrackDistributor(newTrackManager(0, nil), trackID, nil)
	defer close(dist.done)

	// Pre-fill one complete group so cache.next(i) is always a hit. Frame size
	// 1200B is representative of a typical video frame on the wire.
	const frameSize = 1200
	const nFrames = 64
	pool := NewFramePool(DefaultNewFrameCapacity)
	gc := newGroupCache(1)
	src := moqt.NewFrame(frameSize)
	src.Write(make([]byte, frameSize))
	for range nFrames {
		gc.append(src, pool)
	}

	b.ResetTimer()
	b.ReportAllocs()

	var wg sync.WaitGroup
	opsPerSub := b.N / nSubs
	for range nSubs {
		wg.Go(func() {
			var acc int64
			for i := range opsPerSub {
				f := gc.next(i % nFrames)
				if withCounters {
					nn := int64(f.Len())
					if batched {
						// One egressCounter.Add per group (nFrames frames),
						// matching deliverGroup's per-group flush.
						acc += nn
						if i%nFrames == nFrames-1 {
							dist.egressCounter.Add(float64(acc))
							acc = 0
						}
					} else {
						dist.egressCounter.Add(float64(nn))
					}
				}
			}
			if batched && acc > 0 {
				dist.egressCounter.Add(float64(acc)) // flush remainder
			}
		})
	}
	wg.Wait()
}

// BenchmarkEgressAccounting_Fanout measures the per-frame egress accounting
// (next + egressCounter.Add) under increasing fan-out. Hypothesis: the shared
// per-track prometheus.Counter ping-pongs under contention, so ns/op grows super-linearly with subscriber count.
func BenchmarkEgressAccounting_Fanout(b *testing.B) {
	for _, n := range fanoutSizes {
		b.Run(fmt.Sprintf("subs=%d", n), func(b *testing.B) {
			runEgressFanout(b, n, true, false)
		})
	}
}

// BenchmarkEgressAccounting_Fanout_Batched measures the per-group egress
// accounting (#333): egressCounter.Add is flushed once per group of nFrames
// frames instead of once per frame. Under fan-out contention the shared counter
// sees ~nFrames× fewer CAS operations, so ns/op should sit below the per-frame
// variant (BenchmarkEgressAccounting_Fanout) and the gap should widen with
// subscriber count. This is the benchmark that directly measures the #333 win.
func BenchmarkEgressAccounting_Fanout_Batched(b *testing.B) {
	for _, n := range fanoutSizes {
		b.Run(fmt.Sprintf("subs=%d", n), func(b *testing.B) {
			runEgressFanout(b, n, true, true)
		})
	}
}

// BenchmarkEgressRead_Fanout is the contention-free baseline: the same loop
// without the two shared-atomics Add calls. ns/op should stay roughly flat
// across the fan-out sweep. The gap between this and the counters variant is
// the contention cost attributable to the shared counters.
func BenchmarkEgressRead_Fanout(b *testing.B) {
	for _, n := range fanoutSizes {
		b.Run(fmt.Sprintf("subs=%d", n), func(b *testing.B) {
			runEgressFanout(b, n, false, false)
		})
	}
}
