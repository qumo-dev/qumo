package ingest

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
)

// BenchmarkTrackBufferNotify measures notify()'s per-call cost with the
// trackBuffer's notification under load. Pre-broadcastNotify this walked one
// channel per subscriber under an RWMutex; the scenarios here reproduce that
// access pattern against the current design so benchstat can compare across
// the redesign (the full/subs variants are closest to the old full-channel
// case; the old empty/full channel semantics have no direct successor since
// per-subscriber channels no longer exist).
//
//   - nolisten: notify() in a tight loop with no listeners — the writer-side
//     floor (seq bump + channel swap), no waiters to wake.
//   - parked: `subs` goroutines parked waiting on the current channel —
//     notify must close that channel, making the runtime wake all of them.
//     This is the fan-out cost as production pays it: the wake work happens
//     while the writer holds no lock, but it is still timed here as the upper
//     bound of the writer's per-call cost.
func BenchmarkTrackBufferNotify(b *testing.B) {
	for _, n := range []int{1, 10, 100, 1000} {
		b.Run(fmt.Sprintf("nolisten/subs=%d", n), func(b *testing.B) {
			buf := newTestTrackBuffer()
			b.ResetTimer()
			for range b.N {
				buf.broadcast()
			}
			b.StopTimer()
			runtime.GC() // don't leak this bench's heap goal into later benches
		})

		b.Run(fmt.Sprintf("parked/subs=%d", n), func(b *testing.B) {
			buf := newTestTrackBuffer()
			stop := make(chan struct{})
			var wg sync.WaitGroup
			for range n {
				wg.Go(func() {
					for {
						state := buf.notify.listen()
						// Select on stop AND the notify channel: after the final
						// broadcast the current channel never closes, so waiters
						// parked only on it would deadlock the harness at
						// close(stop)/wg.Wait().
						select {
						case <-stop:
							return
						case <-state.ch:
						}
					}
				})
			}
			// Let the waiters park before timing.
			time.Sleep(10 * time.Millisecond)

			b.ResetTimer()
			for range b.N {
				buf.broadcast()
			}
			b.StopTimer()
			close(stop)
			wg.Wait()
			runtime.GC() // don't leak this bench's heap goal into later benches
		})
	}
}

// fanoutSizes is the subscriber-count sweep for the fan-out benchmarks, chosen
// to match the relay's egress-accounting sweep (1 → 100) plus 1000 to expose
// super-linear scaling.
var fanoutSizes = []int{1, 2, 4, 16, 64, 100, 1000}

// BenchmarkTrackBufferNotify_Fanout measures the full push-path wakeup across
// increasing subscriber counts: every iteration opens a group, appends a
// pre-built frame, and calls notify() — the exact per-frame work videoTrack.push
// performs — with `subs` goroutines parked on the notify channel as egress
// goroutines would be. The ns/op is the writer-side cost of one push under
// fan-out; on the pre-broadcastNotify design this grew linearly with subs
// (the RWMutex-locked channel walk), and it should now be roughly flat.
func BenchmarkTrackBufferNotify_Fanout(b *testing.B) {
	for _, n := range fanoutSizes {
		b.Run(fmt.Sprintf("subs=%d", n), func(b *testing.B) {
			buf := newTestTrackBuffer()
			stop := make(chan struct{})
			var wg sync.WaitGroup
			for range n {
				wg.Go(func() {
					for {
						state := buf.notify.listen()
						// Select on stop AND the notify channel — see the parked
						// variant above for why the stop case is required.
						select {
						case <-stop:
							return
						case <-state.ch:
						}
					}
				})
			}
			time.Sleep(10 * time.Millisecond) // let waiters park

			frame := moqt.NewFrame(4)
			frame.Write([]byte{0x27, 0x01, 0x00, 0x00})

			b.ResetTimer()
			for range b.N {
				g := buf.openGroup()
				g.append(frame)
				g.complete.Store(true)
				buf.broadcast()
			}
			b.StopTimer()

			close(stop)
			wg.Wait()
			runtime.GC() // don't leak this bench's heap goal into later benches
		})
	}
}
