//go:build instrument

package relay

import (
	"sync/atomic"
	"time"
)

// Real stage-latency collector (-tags instrument). Lock-free two-tier linear
// histograms: 1µs resolution up to 10ms (service times are µs-scale), 100µs
// resolution up to 1s (queueing times are ms-scale), plus one overflow bucket.
// All observation paths are a single bucket index computation + two atomic
// adds, so the collector itself does not serialize the fan-out it measures.
//
// CAVEAT: an instrumented build perturbs throughput (extra time.Now per frame
// per subscriber). Use it for latency ATTRIBUTION only; throughput and absolute
// e2e latency claims come from the clean build.

const (
	stageFineStep      = time.Microsecond
	stageFineMax       = 10 * time.Millisecond
	stageFineBuckets   = int(stageFineMax / stageFineStep) // 10_000
	stageCoarseStep    = 100 * time.Microsecond
	stageCoarseMax     = time.Second
	stageCoarseBuckets = int((stageCoarseMax - stageFineMax) / stageCoarseStep) // 9_900
	stageBuckets       = stageFineBuckets + stageCoarseBuckets + 1              // + overflow
)

type stageHist struct {
	buckets [stageBuckets]atomic.Int64
	count   atomic.Int64
	max     atomic.Int64
}

func (h *stageHist) observe(d time.Duration) {
	if d < 0 {
		return
	}
	var idx int
	switch {
	case d < stageFineMax:
		idx = int(d / stageFineStep)
	case d < stageCoarseMax:
		idx = stageFineBuckets + int((d-stageFineMax)/stageCoarseStep)
	default:
		idx = stageBuckets - 1
	}
	h.buckets[idx].Add(1)
	h.count.Add(1)
	for {
		cur := h.max.Load()
		if int64(d) <= cur || h.max.CompareAndSwap(cur, int64(d)) {
			return
		}
	}
}

// stageBucketUpper is the inclusive upper bound of bucket idx — the value a
// nearest-rank percentile reports for samples landing in that bucket.
func stageBucketUpper(idx int) time.Duration {
	if idx < stageFineBuckets {
		return time.Duration(idx+1) * stageFineStep
	}
	if idx < stageBuckets-1 {
		return stageFineMax + time.Duration(idx-stageFineBuckets+1)*stageCoarseStep
	}
	return stageCoarseMax // overflow: reported as the ceiling
}

// percentileFromCounts is nearest-rank over a bucket-count snapshot.
func percentileFromCounts(counts []int64, total int64, p float64) time.Duration {
	if total == 0 {
		return 0
	}
	rank := int64(p / 100 * float64(total))
	if rank < 1 {
		rank = 1
	}
	var cum int64
	for i, c := range counts {
		cum += c
		if cum >= rank {
			return stageBucketUpper(i)
		}
	}
	return stageBucketUpper(stageBuckets - 1)
}

func (h *stageHist) snapshot() StageSnapshot {
	counts := make([]int64, stageBuckets)
	var total int64
	for i := range h.buckets {
		c := h.buckets[i].Load()
		counts[i] = c
		total += c
	}
	return StageSnapshot{
		N:   total,
		P50: percentileFromCounts(counts, total, 50),
		P95: percentileFromCounts(counts, total, 95),
		P99: percentileFromCounts(counts, total, 99),
		Max: time.Duration(h.max.Load()),
	}
}

func (h *stageHist) reset() {
	for i := range h.buckets {
		h.buckets[i].Store(0)
	}
	h.count.Store(0)
	h.max.Store(0)
}

// stageCollector (instrument build) records real per-stage histograms. All
// methods are nil-safe so call sites need no guards.
type stageCollector struct {
	ingress stageHist // A: per-frame fill/append service
	ring    stageHist // R: group arrival → deliverGroup entry, per subscriber
	open    stageHist // O: OpenGroupAt duration
	egress  stageHist // C: per-frame WriteFrame service

	// Mechanism investigation (A vs B). See StageReport for semantics.
	rFill       stageHist // reserve → first broadcast (fill/ingest latency)
	rWake       stageHist // first broadcast → pickup (egress wake+schedule)
	rBehind     stageHist // R when picked up directly (subscriber behind)
	rWoken      stageHist // R when picked up after a notify wait (caught up)
	deliverSpan stageHist // deliverGroup entry → end, per subscriber
	broadcastD  stageHist // broadcast() wall time
	fillSem     stageHist // processGroup fill-slot acquisition wait

	broadcastN     atomic.Int64
	inDeliver      atomic.Int64 // egress goroutines currently in deliverGroup
	maxInDeliver   atomic.Int64
	activeGroups   atomic.Int64 // group generations currently being delivered
	maxActiveGroup atomic.Int64

	// interArrival records the spacing between consecutive group reserves as the
	// relay sees them (publisher group-open cadence). Even spacing ≈ gap = paced
	// real-time arrival; a spike of near-zero deltas = burst group creation.
	// Single-track assumption: lastReserveNano is swapped per reserve on the one
	// ingest goroutine, so for a multi-track server this interleaves tracks.
	interArrival    stageHist
	lastReserveNano atomic.Int64
}

// storeMax atomically raises *dst to v if v is larger.
func storeMax(dst *atomic.Int64, v int64) {
	for {
		cur := dst.Load()
		if v <= cur || dst.CompareAndSwap(cur, v) {
			return
		}
	}
}

// groupBroadcast stamps the instant fill first published this generation's data
// (CAS 0→now, so only the first frame's broadcast counts). It is the boundary
// that splits ring residence into fill latency and egress wake latency.
func (c *stageCollector) groupBroadcast(gc *groupCache) {
	if c == nil || gc == nil {
		return
	}
	gc.firstBroadcastNano.CompareAndSwap(0, monoNanos())
}

// ringResidenceSplit records the aggregate R plus its fill/wake split and the
// behind/woken classification, for one egress pickup of gc at time t.
func (c *stageCollector) ringResidenceSplit(gc *groupCache, t time.Time, woken bool) {
	if c == nil || gc == nil || t.IsZero() {
		return
	}
	arrival := gc.ingressArrivalNano.Load()
	if arrival == 0 {
		return
	}
	at := int64(t.Sub(monoEpoch)) // t on monoNanos's clock
	r := time.Duration(at - arrival)
	c.ring.observe(r)
	if woken {
		c.rWoken.observe(r)
	} else {
		c.rBehind.observe(r)
	}
	if bc := gc.firstBroadcastNano.Load(); bc != 0 {
		c.rFill.observe(time.Duration(bc - arrival))
		c.rWake.observe(time.Duration(at - bc))
	}
}

// enterDeliver marks one egress goroutine entering deliverGroup for gc. The
// first goroutine to pick up a generation (CAS on firstPickupNano) bumps the
// concurrent-active-groups gauge; every entry bumps the concurrent-deliveries
// gauge.
func (c *stageCollector) enterDeliver(gc *groupCache) {
	if c == nil {
		return
	}
	storeMax(&c.maxInDeliver, c.inDeliver.Add(1))
	if gc != nil && gc.firstPickupNano.CompareAndSwap(0, time.Now().UnixNano()) {
		storeMax(&c.maxActiveGroup, c.activeGroups.Add(1))
	}
}

// exitDeliver records the deliver span and releases the concurrency gauge.
func (c *stageCollector) exitDeliver(gc *groupCache, t0 time.Time) {
	if c == nil {
		return
	}
	c.inDeliver.Add(-1)
	if gc != nil {
		storeMax(&gc.lastEndNano, time.Now().UnixNano())
	}
	if !t0.IsZero() {
		c.deliverSpan.observe(time.Since(t0))
	}
}

// groupReleased decrements the active-groups gauge for a generation that was
// delivered (had at least one pickup). Called from releaseCache.
func (c *stageCollector) groupReleased(gc *groupCache) {
	if c == nil || gc == nil {
		return
	}
	if gc.firstPickupNano.Load() != 0 {
		c.activeGroups.Add(-1)
	}
}

func (c *stageCollector) broadcastTimed(t0 time.Time) {
	if c == nil || t0.IsZero() {
		return
	}
	c.broadcastD.observe(time.Since(t0))
	c.broadcastN.Add(1)
}

func (c *stageCollector) fillSemWaited(t0 time.Time) {
	if c == nil || t0.IsZero() {
		return
	}
	c.fillSem.observe(time.Since(t0))
}

func (c *stageCollector) now() time.Time {
	if c == nil {
		return time.Time{}
	}
	return time.Now()
}

func (c *stageCollector) ingressFrame(t0 time.Time) {
	if c == nil || t0.IsZero() {
		return
	}
	c.ingress.observe(time.Since(t0))
}

// stampArrival records the group-open cadence from the arrival instant
// reserve stamped (monoNanos's clock).
func (c *stageCollector) stampArrival(gc *groupCache) {
	if c == nil || gc == nil {
		return
	}
	now := gc.ingressArrivalNano.Load()
	// Record the group-open cadence (spacing since the previous reserve).
	if prev := c.lastReserveNano.Swap(now); prev != 0 && now > prev {
		c.interArrival.observe(time.Duration(now - prev))
	}
}

// clearArrival zeroes a reused cache's stale instrument stamps so the
// mechanism gauges never measure against a previous generation. Called from reserve; the
// default build's no-op keeps the reserve path free of this cost.
func (c *stageCollector) clearArrival(gc *groupCache) {
	if c == nil || gc == nil {
		return
	}
	gc.firstBroadcastNano.Store(0)
	gc.firstPickupNano.Store(0)
	gc.lastEndNano.Store(0)
}

func (c *stageCollector) groupOpen(t0 time.Time) {
	if c == nil || t0.IsZero() {
		return
	}
	c.open.observe(time.Since(t0))
}

func (c *stageCollector) egressFrame(t0 time.Time) {
	if c == nil || t0.IsZero() {
		return
	}
	c.egress.observe(time.Since(t0))
}

func (c *stageCollector) report() *StageReport {
	if c == nil {
		return nil
	}
	return &StageReport{
		IngressService: c.ingress.snapshot(),
		RingResidence:  c.ring.snapshot(),
		GroupOpen:      c.open.snapshot(),
		EgressService:  c.egress.snapshot(),

		RingFill:                c.rFill.snapshot(),
		RingWake:                c.rWake.snapshot(),
		RingBehind:              c.rBehind.snapshot(),
		RingWoken:               c.rWoken.snapshot(),
		DeliverSpan:             c.deliverSpan.snapshot(),
		BroadcastDur:            c.broadcastD.snapshot(),
		FillSemWait:             c.fillSem.snapshot(),
		BroadcastN:              c.broadcastN.Load(),
		MaxConcurrentGroups:     c.maxActiveGroup.Load(),
		MaxConcurrentDeliveries: c.maxInDeliver.Load(),
		GroupInterArrival:       c.interArrival.snapshot(),
	}
}

func (c *stageCollector) reset() {
	if c == nil {
		return
	}
	c.ingress.reset()
	c.ring.reset()
	c.open.reset()
	c.egress.reset()

	c.rFill.reset()
	c.rWake.reset()
	c.rBehind.reset()
	c.rWoken.reset()
	c.deliverSpan.reset()
	c.broadcastD.reset()
	c.fillSem.reset()
	c.broadcastN.Store(0)
	c.inDeliver.Store(0)
	c.maxInDeliver.Store(0)
	c.activeGroups.Store(0)
	c.maxActiveGroup.Store(0)
	c.interArrival.reset()
	c.lastReserveNano.Store(0)
}
