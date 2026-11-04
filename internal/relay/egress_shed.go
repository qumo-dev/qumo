package relay

import (
	"context"
	"runtime/metrics"
	"sync/atomic"
	"time"
)

// egressShedInterval is how often the shedder reads the heap. Under the
// overload it guards against, the send backlog grows by ~100 MB/s on a
// 4-core relay (docs/perf/sakura-eval-2026-11.md), so a reading every 100 ms
// keeps the overshoot past the limit to a few tens of MB.
const egressShedInterval = 100 * time.Millisecond

// egressShedResumeRatio is the share of the limit the heap must fall below
// before shedding stops: the hysteresis that keeps the shedder from
// flapping around the limit on every reading.
const egressShedResumeRatio = 0.8

// heapObjectsMetric is the runtime/metrics name of the bytes held by live
// and not-yet-swept heap objects: what the QUIC send backlog occupies.
const heapObjectsMetric = "/memory/classes/heap/objects:bytes"

// egressShed drops new groups at every subscriber while the heap is past a
// limit, so that a relay pushed past its CPU stops opening streams it cannot
// send instead of queueing them until it is killed for memory.
//
// The relay opens one QUIC stream per group. Its only backpressure on that
// is the subscriber's MAX_STREAMS (OpenGroupAt blocks, then times out and
// drops the group); a subscriber that grants many streams grants the relay
// an unbounded queue. When the relay runs out of CPU, quic-go's send loops
// fall behind the egress goroutines that open streams, and every opened but
// unsent stream holds a packet-sized buffer, its SendStream and its flow
// controller: memory grows linearly with time until the process is killed.
// quic-go exposes no per-connection send-queue depth to bound this per
// session, so the bound is taken on what actually fails, the heap.
//
// Shedding is relay-wide, not per subscriber: the overload it answers is
// the whole relay's CPU, and every subscriber of a group shares its fate.
// It is off unless RELAY_EGRESS_SHED_HEAP_MB is set.
type egressShed struct {
	limit  uint64 // bytes; shedding starts above it
	resume uint64 // bytes; shedding stops below it

	active atomic.Bool
	// read returns the heap's object bytes; runtime/metrics in production.
	read func() uint64
}

// newEgressShed returns a shedder for a heap limit in bytes, or nil (no
// shedding; every method is nil-safe) when limit is 0.
func newEgressShed(limit uint64) *egressShed {
	if limit == 0 {
		return nil
	}
	return &egressShed{
		limit:  limit,
		resume: uint64(float64(limit) * egressShedResumeRatio),
		read:   readHeapObjects,
	}
}

// shedding reports whether new groups are being dropped now. One atomic
// load: it is on every group delivery.
func (e *egressShed) shedding() bool {
	return e != nil && e.active.Load()
}

// update reads the heap once and starts or stops shedding.
func (e *egressShed) update() {
	heap := e.read()
	switch {
	case !e.active.Load() && heap > e.limit:
		e.active.Store(true)
		metricEgressShedding.Set(1)
		metricEgressShedEpisodesTotal.Inc()
	case e.active.Load() && heap < e.resume:
		e.active.Store(false)
		metricEgressShedding.Set(0)
	}
}

// run reads the heap every egressShedInterval until ctx ends.
func (e *egressShed) run(ctx context.Context) {
	if e == nil {
		return
	}
	t := time.NewTicker(egressShedInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.update()
		}
	}
}

func readHeapObjects() uint64 {
	s := []metrics.Sample{{Name: heapObjectsMetric}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return s[0].Value.Uint64()
}
