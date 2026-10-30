package relay

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/qumo-dev/gomoqt/moqt"
	"golang.org/x/sync/singleflight"
)

// defaultGroupTimeout bounds how long OpenGroupAt may block waiting for a QUIC
// stream slot (MAX_STREAMS backpressure). When the peer can't accept a new
// stream within this window, the group is dropped rather than blocking the
// egress goroutine indefinitely. Should be generous enough to absorb normal
// scheduling jitter (GC pauses, ACK round-trips) — 30ms ≈ one frame at 30fps
// or ~15 group intervals at 500fps. Counterintuitively, a longer timeout
// degrades throughput at high fan-out: more egress goroutines block
// simultaneously in quic-go's stream multiplexer, holding goroutine stacks
// and stream objects longer, which increases memory pressure and GC cost.
var defaultGroupTimeout = 30 * time.Millisecond

// DrainTimeout is the grace period given to a displaced relayHandler before
// its upstream context is cancelled. During this window existing subscribers
// can finish reading in-flight groups before the upstream subscription stops.
var DrainTimeout = 30 * time.Second

// MaxGroupFillsInFlight is the maximum number of fill goroutines that a
// single trackDistributor may run simultaneously. It caps concurrent fill
// work and prevents unbounded goroutine growth under bursty or
// slow-consumer conditions. If the limit is reached, new fill goroutines wait
// for a slot after AcceptGroup returns; this does not apply backpressure at
// AcceptGroup itself.
// The default is max(32, 2×GOMAXPROCS); override before calling Relay.
// Must be >= 1; newTrackDistributor panics otherwise.
var MaxGroupFillsInFlight = max(32, 2*runtime.GOMAXPROCS(0))

var errTrackNotFound = errors.New("track not found")

// StopRedirect is a CheckRedirect that follows no redirect: Session.Subscribe
// returns each one as a *moqt.RedirectError. The relay's sessions use it, so
// that ServeTrack passes a publisher's redirect on rather than following it
// upstream, and so does the HLS egress, which follows redirects itself.
func StopRedirect(moqt.Redirect, []moqt.Redirect) error { return moqt.ErrStopRedirect }

// maxGroupFillsInFlightOrPanic returns MaxGroupFillsInFlight, panicking if it
// is less than 1 so that misconfiguration causes an immediate, clear failure
// rather than a silent deadlock.
func maxGroupFillsInFlightOrPanic() int {
	if MaxGroupFillsInFlight < 1 {
		panic("relay: MaxGroupFillsInFlight must be >= 1")
	}
	return MaxGroupFillsInFlight
}

var _ moqt.TrackHandler = (*relayHandler)(nil)
var _ moqt.TrackInfoProvider = (*relayHandler)(nil)
var _ RouteReporter = (*relayHandler)(nil)
var _ Drainable = (*relayHandler)(nil)

// RouteStats holds routing quality metrics for a relayed broadcast path.
type RouteStats struct {
	// Alive reports whether the upstream session is still connected.
	// A false value means the handler is dead and must be replaced unconditionally.
	Alive bool
	// Hops is the number of relay hops the announcement has traversed.
	// Fewer hops generally implies lower latency.
	Hops int
	// EstimatedBitrate is the measured bitrate in bits per second. A value of 0 means unknown.
	EstimatedBitrate uint64
	// RTT is the smoothed round-trip time in milliseconds. A value of 0 means unknown.
	RTT time.Duration
}

// Drainable is implemented by handlers that support graceful drain-then-shutdown.
// When a handler is displaced by a better route, Drain is called so that
// in-flight streams can complete before the upstream subscription is torn down.
type Drainable interface {
	// Drain schedules cancellation of the handler's context after timeout.
	// It is idempotent: only the first call schedules a timer; subsequent calls
	// are no-ops. The handler's context is cancelled once when the timer fires.
	Drain(timeout time.Duration)
}

// RouteReporter is implemented by handlers that can report routing quality
// metrics for a relayed broadcast path. Use a type assertion on the
// TrackHandler returned by TrackMux.TrackHandler:
//
//	_, h := mux.TrackHandler(path)
//	if rr, ok := h.(relay.RouteReporter); ok {
//		stats := rr.RouteStats()
//	}
//
// Evaluation is intentionally performed only when a new route candidate
// arrives, not periodically, to preserve cache hit rates and playback
// continuity.
type RouteReporter interface {
	// RouteStats probes the upstream session once and returns combined
	// routing metrics. Called at most once per route comparison.
	RouteStats() RouteStats
}

type relayHandler struct {
	announcement *moqt.Announcement
	session      *moqt.Session

	tracks  *trackManager
	flights singleflight.Group
	nodeID  string

	trackInfoCache sync.Map           // moqt.TrackName -> moqt.PublishInfo
	infoFlights    singleflight.Group // deduplicates concurrent upstream TrackInfo queries

	// sampler is the server-wide stats sampler, passed to each track
	// distributor this handler creates (see subscribe). nil is valid (nil-safe
	// methods) for a handler built without a Server.
	sampler *statsSampler

	ctx       context.Context
	cancel    context.CancelFunc
	drainOnce sync.Once
}

// routeDecision is the result of compareRoutes, encoding both the better/worse
// verdict and the specific dimension that decided it. Values up to
// decisionRTT are accept decisions (candidate is better); values from
// decisionDeadCandidate onward are reject decisions.
// String returns the metric label value for both cases.
type routeDecision int

const (
	// Accept decisions: candidate is better.
	// WARNING: accepted() (line 143) relies on all accept values preceding all
	// reject values in this iota block. Insert new values above the divider,
	// never between accept and reject.
	decisionAlive   routeDecision = iota // candidate is alive while current is dead
	decisionHops                         // candidate has fewer hops
	decisionBitrate                      // candidate has significantly higher bitrate
	decisionRTT                          // candidate has significantly lower RTT

	// Reject decisions: candidate is not better.
	decisionDeadCandidate   // candidate is not alive
	decisionInferiorHops    // candidate has more hops
	decisionInferiorBitrate // candidate has lower bitrate
	decisionInferiorRTT     // candidate has higher or equal RTT
	decisionEqualOrUnknown  // RTT unknown (0) for one or both routes

	decisionLowBitrateMargin // candidate has higher bitrate but not by enough margin
	decisionLowRTTMargin     // candidate has lower RTT but not by enough margin
)

// accepted reports whether this decision means the candidate route is better.
func (d routeDecision) accepted() bool { return d <= decisionRTT }

// String returns the metric label value for this decision.
func (d routeDecision) String() string {
	switch d {
	case decisionAlive:
		return "alive"
	case decisionHops:
		return "hops"
	case decisionBitrate:
		return "bitrate"
	case decisionRTT:
		return "rtt"
	case decisionDeadCandidate:
		return "dead_candidate"
	case decisionInferiorHops:
		return "inferior_hops"
	case decisionInferiorBitrate:
		return "inferior_bitrate"
	case decisionInferiorRTT:
		return "inferior_rtt"
	case decisionEqualOrUnknown:
		return "equal_or_unknown"
	case decisionLowBitrateMargin:
		return "insufficient_bitrate_margin"
	case decisionLowRTTMargin:
		return "insufficient_rtt_margin"
	default:
		return "unknown"
	}
}

// Hysteresis thresholds for route replacement. When routes have equal hops,
// the candidate must clear these margins to displace the current route.
// This prevents all edges from converging on the same hub due to small,
// transient metric differences. Hops and liveness comparisons remain strict
// (structural, not noisy).
const (
	// bitrateMargin is the fractional improvement in EstimatedBitrate
	// required to displace the current route on bitrate alone.
	// e.g., 1.2 means the candidate must have >20% higher bitrate.
	bitrateMargin = 1.2

	// rttMarginAbs is the minimum absolute RTT improvement required
	// to displace the current route. Prevents churn from sub-ms noise.
	rttMarginAbs = 5 * time.Millisecond

	// rttMarginRel is the fractional improvement in RTT required
	// to displace the current route. e.g., 0.8 means candidate RTT must be
	// less than 80% of current RTT.
	rttMarginRel = 0.8
)

// compareRoutes reports whether candidate is a strictly better route than
// current, returning a routeDecision that encodes both the verdict and the
// winning (or losing) dimension for logging and metrics.
//
// A live route always beats a dead one. Among routes with the same liveness,
// fewer hops wins outright; equal hops are broken first by bitrate (higher
// available bandwidth is better for streaming), then by RTT (lower latency is
// better). Bitrate and RTT comparisons apply hysteresis thresholds so that
// small, transient metric differences do not cause route churn. When a metric
// cannot be determined (nil probe or 0 value), the current route is preferred.
func compareRoutes(candidate, current RouteStats) routeDecision {
	// A live route always beats a dead one.
	if candidate.Alive != current.Alive {
		if candidate.Alive {
			return decisionAlive
		}
		return decisionDeadCandidate
	}
	// Both dead: no benefit in switching.
	if !candidate.Alive {
		return decisionDeadCandidate
	}
	if candidate.Hops < current.Hops {
		return decisionHops
	}
	if candidate.Hops > current.Hops {
		return decisionInferiorHops
	}

	// Equal hops: bitrate with hysteresis.
	if candidate.EstimatedBitrate != current.EstimatedBitrate {
		if float64(candidate.EstimatedBitrate) >= float64(current.EstimatedBitrate)*bitrateMargin {
			return decisionBitrate
		}
		if candidate.EstimatedBitrate > current.EstimatedBitrate {
			return decisionLowBitrateMargin
		}
		return decisionInferiorBitrate
	}

	// Bitrate equal or unknown: RTT with hysteresis.
	if candidate.RTT == 0 || current.RTT == 0 {
		return decisionEqualOrUnknown
	}
	candidateBetter := candidate.RTT < current.RTT
	absImprovement := current.RTT - candidate.RTT
	relImprovement := float64(candidate.RTT) / float64(current.RTT)

	if candidateBetter && (absImprovement >= rttMarginAbs || relImprovement <= rttMarginRel) {
		return decisionRTT
	}
	if candidateBetter {
		return decisionLowRTTMargin
	}
	return decisionInferiorRTT
}

func newRelayHandler(ann *moqt.Announcement, sess *moqt.Session, nodeID string, cacheSize int, pool *FramePool, sampler *statsSampler) *relayHandler {
	if sess == nil {
		panic("relay: session must not be nil")
	}
	if ann == nil {
		return nil
	}

	ctx, cancel := context.WithCancel(sess.Context())
	h := &relayHandler{
		announcement: ann,
		session:      sess,
		tracks:       newTrackManager(cacheSize, pool),
		nodeID:       nodeID,
		sampler:      sampler,
		ctx:          ctx,
		cancel:       cancel,
	}

	// Release this handler's child context from the session's children list when
	// the session ends. The ctx is already derived from the session ctx (so it
	// is cancelled on session end regardless); this is a cleanup optimization,
	// not correctness. Registered here, where sess is guaranteed non-nil, so
	// installRoute need not handle a nil session context.
	context.AfterFunc(sess.Context(), cancel)

	return h
}

// Drain schedules cancellation of this handler's context after timeout.
// New subscribers will find no active handler; existing in-flight groups
// are allowed to finish within the grace window.
// It is idempotent: only the first call schedules a timer; subsequent calls
// are no-ops and do not create additional goroutines.
func (h *relayHandler) Drain(timeout time.Duration) {
	h.drainOnce.Do(func() {
		time.AfterFunc(timeout, h.cancel)
	})
}

// RouteStats probes the upstream session and returns combined routing metrics.
// The probe is performed once per call; results are not cached.
func (h *relayHandler) RouteStats() RouteStats {
	sessionStats := h.session.Stats()

	rs := RouteStats{
		Alive:            h.ctx.Err() == nil && h.announcement.IsActive(),
		Hops:             len(h.announcement.HopIDs()),
		EstimatedBitrate: sessionStats.EstimatedBitrate,
		RTT:              sessionStats.RTT,
	}

	return rs
}

// TrackInfo implements moqt.TrackInfoProvider by querying the upstream session
// for the track's immutable publisher properties (TRACK_INFO), caching the result
// and deduplicating concurrent upstream queries with singleflight.
func (h *relayHandler) TrackInfo(name moqt.TrackName) (pubInfo moqt.PublishInfo, ok bool) {
	// Fast path: cache hit
	if val, found := h.trackInfoCache.Load(name); found {
		return val.(moqt.PublishInfo), true
	}

	// Capture local snapshots for thread-safety and stable references
	session := h.session
	announcement := h.announcement
	ctx := h.ctx

	if session == nil || announcement == nil || !announcement.IsActive() || ctx.Err() != nil {
		return moqt.PublishInfo{}, false
	}

	path := announcement.BroadcastPath()

	defer func() {
		if r := recover(); r != nil {
			pubInfo = moqt.PublishInfo{}
			ok = false
		}
	}()

	res, err, _ := h.infoFlights.Do(string(name), func() (any, error) {
		// Double-check cache inside flight
		if val, found := h.trackInfoCache.Load(name); found {
			return val.(moqt.PublishInfo), nil
		}
		info, err := session.TrackInfo(ctx, path, name)
		if err != nil || info == nil {
			return nil, errTrackNotFound
		}
		h.trackInfoCache.Store(name, *info)
		return *info, nil
	})
	if err != nil || res == nil {
		return moqt.PublishInfo{}, false
	}
	return res.(moqt.PublishInfo), true
}

func (h *relayHandler) ServeTrack(tw *moqt.TrackWriter) {
	logger := slog.With(
		"node", h.nodeID,
		"broadcast_path", tw.BroadcastPath,
		"track_name", tw.TrackName,
	)

	// Fast path: reuse existing distributor
	trackID := "[" + h.nodeID + "]" + string(tw.BroadcastPath) + "/" + string(tw.TrackName)
	if d, ok := h.tracks.load(trackID); ok {
		metricTrackDistributorReusesTotal.WithLabelValues(string(tw.BroadcastPath), string(tw.TrackName)).Inc()
		logger.Debug("relay: ServeTrack fast path — reusing distributor", "track_id", trackID)
		d.egress(tw, false)
		return
	}

	logger.Debug("relay: ServeTrack — no existing distributor, will subscribe upstream", "track_id", trackID)

	// Dedup: only one upstream subscribe per track name at a time
	ch := h.flights.DoChan(string(tw.TrackName), func() (any, error) {
		return h.subscribe(tw.TrackName)
	})

	select {
	case result := <-ch:
		if redirectErr, ok := errors.AsType[*moqt.RedirectError](result.Err); ok {
			h.passRedirect(tw, redirectErr.Redirect, logger)
			return
		}
		if result.Err != nil {
			tw.CloseWithError(moqt.SubscribeErrorCodeNotFound)
			metricSubscribeErrorsTotal.WithLabelValues("not_found").Inc()
			logger.Warn("Track not found, closing track writer")
			return
		}
		up := result.Val.(upstreamTrack)
		up.d.egress(tw, up.opened)
	case <-tw.Context().Done():
		// Client unsubscribed before we could subscribe upstream - just return
		return
	}
}

// passRedirect answers tw with the redirect its publisher answered the
// relay's upstream subscribe with. The target is subscribed through this
// relay as any other track. A redirect to another relay is refused: the relay
// does not send its subscribers to a host its operator did not choose.
func (h *relayHandler) passRedirect(tw *moqt.TrackWriter, to moqt.Redirect, logger *slog.Logger) {
	if to.URI != "" {
		tw.CloseWithError(moqt.SubscribeErrorCodeNotFound)
		metricSubscribeRedirectsTotal.WithLabelValues("refused").Inc()
		logger.Warn("relay: refused a publisher's redirect to another relay", "to_broadcast", to.BroadcastPath)
		return
	}
	if err := tw.Redirect(tw.Context(), to); err != nil {
		tw.CloseWithError(moqt.SubscribeErrorCodeNotFound)
		metricSubscribeRedirectsTotal.WithLabelValues("not_passed").Inc()
		logger.Warn("relay: could not pass a redirect on", "to", to.BroadcastPath, "error", err)
		return
	}
	metricSubscribeRedirectsTotal.WithLabelValues("passed").Inc()
	logger.Debug("relay: passed a redirect on", "to", to.BroadcastPath, "track", to.TrackName)
}

// upstreamTrack is the result of a deduplicated upstream subscribe: the
// distributor, and whether this flight opened its upstream subscription (as
// opposed to finding a distributor already running).
type upstreamTrack struct {
	d      *trackDistributor
	opened bool
}

// subscribe returns the distributor for name, opening the upstream
// subscription if none is running. It returns the publisher's
// *moqt.RedirectError when the publisher redirected the subscription, and
// errTrackNotFound when the track cannot be subscribed.
func (h *relayHandler) subscribe(name moqt.TrackName) (upstreamTrack, error) {
	announcement := h.announcement
	if announcement == nil {
		slog.Warn("relay: subscribe failed: announcement is nil", "track", name)
		return upstreamTrack{}, errTrackNotFound
	}

	trackID := "[" + h.nodeID + "]" + string(announcement.BroadcastPath()) + "/" + string(name)
	if d, ok := h.tracks.load(trackID); ok {
		return upstreamTrack{d: d}, nil
	}

	session := h.session
	if session == nil {
		slog.Warn("relay: subscribe failed: session is nil", "track", name)
		return upstreamTrack{}, errTrackNotFound
	}

	if !announcement.IsActive() {
		slog.Warn("relay: subscribe failed: announcement inactive",
			"track", name,
			"broadcast_path", announcement.BroadcastPath())
		return upstreamTrack{}, errTrackNotFound
	}

	slog.Debug("relay: subscribe — subscribing upstream",
		"node", h.nodeID,
		"broadcast_path", announcement.BroadcastPath(),
		"track", name,
		"track_id", trackID,
		"announcement_active", announcement.IsActive(),
	)

	path := string(announcement.BroadcastPath())
	track := string(name)
	metricTrackUpstreamRequestsTotal.WithLabelValues(path, track).Inc()
	start := time.Now()
	src, err := session.Subscribe(h.ctx, announcement.BroadcastPath(), name, nil)
	metricTrackUpstreamRequestDuration.WithLabelValues(path, track).Observe(time.Since(start).Seconds())
	if _, ok := errors.AsType[*moqt.RedirectError](err); ok {
		return upstreamTrack{}, err
	}
	if err != nil {
		metricTrackUpstreamRequestErrorsTotal.WithLabelValues(path, track).Inc()
		slog.Warn("relay: upstream subscribe failed",
			"node", h.nodeID,
			"broadcast_path", announcement.BroadcastPath(),
			"track", name,
			"error", err)
		return upstreamTrack{}, errTrackNotFound
	}

	d := newTrackDistributor(h.tracks, trackID, h.sampler)

	slog.Debug("relay: starting ingest loop",
		"node", h.nodeID,
		"track_id", trackID,
	)
	go d.ingest(h.ctx, src)

	h.tracks.store(trackID, d)

	return upstreamTrack{d: d, opened: true}, nil
}

// trackManager manages the set of active track distributors.
type trackManager struct {
	m sync.Map // trackID string → *trackDistributor

	// cacheSize is the per-track group ring capacity; pool recycles frame
	// buffers. Both come from the relay Config; ≤0 / nil fall back to the
	// package defaults so a minimally-constructed Server (e.g. tests) works.
	cacheSize int
	pool      *FramePool
}

func newTrackManager(cacheSize int, pool *FramePool) *trackManager {
	if cacheSize <= 0 {
		cacheSize = DefaultGroupCacheSize
	}
	if pool == nil {
		pool = DefaultFramePool
	}
	return &trackManager{cacheSize: cacheSize, pool: pool}
}

func (tm *trackManager) load(trackID string) (*trackDistributor, bool) {
	v, ok := tm.m.Load(trackID)
	if !ok {
		return nil, false
	}
	return v.(*trackDistributor), true
}

func (tm *trackManager) store(trackID string, d *trackDistributor) {
	tm.m.Store(trackID, d)
}

func (tm *trackManager) remove(trackID string, d *trackDistributor) {
	tm.m.CompareAndDelete(trackID, d)
}

// fillJob is a unit of fill work dispatched to a pool worker goroutine. Using
// a struct value instead of a closure avoids a heap allocation per group on the
// ingest hot path.
type fillJob struct {
	src   frameSource
	cache *groupCache
}

type trackDistributor struct {
	trackID string
	ring    *groupRing
	manager *trackManager

	// Pre-bound Prometheus counters to avoid per-frame label lookups in hot paths.
	ingressCounter    prometheus.Counter
	egressCounter     prometheus.Counter
	deliveryHistogram prometheus.Observer

	// sampler is the server-wide stats sampler this distributor registers with
	// so its ring depth is refreshed on the shared sampling tick (replacing a
	// per-distributor pollCacheDepth goroutine). Passed to newTrackDistributor.
	// nil is valid — its methods are nil-safe — so a distributor built without a
	// Server (tests) simply skips depth sampling.
	sampler *statsSampler

	// fillSem is a buffered-channel semaphore acquired BEFORE reserving a ring
	// slot. This guarantees we never leak a reserved cache on context
	// cancellation — the send may block but no ring slot has been consumed.
	// Capacity is MaxGroupFillsInFlight.
	fillSem chan struct{}

	// fillJobs dispatches fill work to a fixed pool of long-lived worker
	// goroutines, replacing a per-group go func(). Its capacity equals
	// MaxGroupFillsInFlight so a send after acquiring fillSem always succeeds
	// immediately (in-flight jobs ≤ capacity). fillWg tracks the workers for
	// clean shutdown.
	fillJobs chan fillJob
	fillWg   sync.WaitGroup

	// notify broadcasts new-data events to all egress goroutines. Each call to
	// broadcast() atomically increments a sequence number and closes the
	// previous notification channel, waking all goroutines waiting on it. This
	// replaces the former per-subscriber []chan struct{} pattern, eliminating
	// O(N) channel sends per broadcast and the associated RWMutex contention.
	notify broadcastNotify

	// stages is the server-wide stage-latency collector (nil-safe; no-op in the
	// default build — see stage_latency.go). Shared with ring for ingress stamps.
	stages *stageCollector

	drops *dropDistributor
	done  chan struct{} // closed when ingest returns
}

func newTrackDistributor(manager *trackManager, trackID string, sampler *statsSampler) *trackDistributor {
	nWorkers := maxGroupFillsInFlightOrPanic()
	d := &trackDistributor{
		trackID:           trackID,
		ring:              newGroupRing(manager.cacheSize, manager.pool),
		drops:             newDropDistributor(dropRingSize),
		manager:           manager,
		ingressCounter:    metricRelayIngressBytesTotal.WithLabelValues(trackID),
		egressCounter:     metricRelayEgressBytesTotal.WithLabelValues(trackID),
		deliveryHistogram: metricGroupDeliveryHistogram.WithLabelValues(trackID),
		sampler:           sampler,
		fillSem:           make(chan struct{}, nWorkers),
		fillJobs:          make(chan fillJob, nWorkers),
		stages:            sampler.stagesRef(),
		done:              make(chan struct{}),
	}
	d.ring.stages = d.stages
	// Fixed worker pool: these goroutines live for the lifetime of the
	// distributor, eliminating per-group goroutine creation/destruction and the
	// per-fill closure allocation. Concurrency is still bounded to nWorkers
	// (one in-flight job per worker) via fillSem, matching the former bound.
	d.fillWg.Add(nWorkers)
	for range nWorkers {
		go d.fillWorker()
	}
	d.sampler.addTrack(d.trackID, d)
	return d
}

// sampleCacheDepth refreshes the ring-depth gauge once. It is invoked by the
// server-wide statsSampler on each tick; the per-track poller goroutine it
// replaced is gone (see statsSampler). The metric series is deleted when the
// distributor deregisters (statsSampler.removeTrack), called from ingest.
func (d *trackDistributor) sampleCacheDepth() {
	head := d.ring.head()
	earliest := d.ring.earliestAvailable()
	depth := 0
	if head >= earliest {
		depth = int(head - earliest + 1)
	}
	metricBufferDepthGroups.WithLabelValues(d.trackID).Set(float64(depth))
}

// egress delivers the track's groups to one subscriber until it leaves or the
// track ends. fromStart is set for the subscribers whose subscribe opened the
// upstream subscription: they start at the oldest group the ring holds, since
// every group in it was fetched for them. The others join at the live edge,
// the newest group.
func (d *trackDistributor) egress(tw *moqt.TrackWriter, fromStart bool) {
	twCtx := tw.Context()

	metricSubscribersActive.Inc()
	defer metricSubscribersActive.Dec()
	path := tw.BroadcastPath.String()
	track := string(tw.TrackName)
	metricTrackSubscriptionsActive.WithLabelValues(path, track).Inc()
	defer metricTrackSubscriptionsActive.WithLabelValues(path, track).Dec()

	// drops reports groups this subscriber will not receive. It runs on this
	// goroutine: upstream drops wake the same notify as new groups, and are
	// flushed after each delivered group and at every wait.
	drops := newDropReporter(tw, d.drops)

	// Track the last seen notify sequence so we can detect new data without
	// per-subscriber channels or RWMutex contention. Each call to broadcast()
	// advances the sequence and closes the previous notification channel;
	// listen() returns both atomically so there is no race between reading the
	// sequence and waiting on the channel.
	lastState := d.notify.listen()

	// Reusable OpenGroupAt deadline for this subscriber (see openTimeout):
	// avoids a context.WithTimeout construction per delivered group.
	openTO := newOpenTimeout(twCtx)
	defer openTO.release()

	last := d.startPosition(fromStart)

	// woken classifies how this subscriber reached the next group (mechanism
	// instrumentation; free in the default build). true = picked up after a
	// notify wait (subscriber was caught up; the pickup delay is wake+schedule
	// latency); false = picked up directly in a delivery loop (subscriber was
	// behind, still draining queued groups → self-serialization). The first
	// pickup after startup counts as caught up.
	woken := true
	// skipped is set when the subscriber jumped ahead in the ring, past
	// groups that were evicted before it could send them.
	skipped := false

	for {
		latest := d.ring.head()

		if last < latest {
			last++

			if earliest := d.ring.earliestAvailable(); last < earliest {
				slog.Debug("subscriber fell behind; skipping groups",
					"requested_position", last,
					"earliest_position", earliest,
					"latest_position", latest,
				)
				metricSubscriberSkipsTotal.Inc()
				last = latest
				skipped = true
			}

			if cache := d.ring.get(last); cache != nil {
				// Read the sequence while get's reference pins the slot:
				// deliverGroup releases it, and the ring may then reuse the
				// slot for a later group.
				seq := cache.seq

				if skipped {
					// Which groups were evicted is unknown, and groups arrive
					// out of order, so an evicted group may be above seq. Report
					// every group reserved up to this one that was neither sent
					// nor reported, except seq, which is sent next. The bound is
					// the cache's own highestSeq, not the ring's: groups that
					// arrived after this one are still in the ring and will be
					// sent.
					drops.reportSkipped(seq, cache.highestSeq, moqt.SubscribeErrorCodeInternal)
					skipped = false
				}

				// A group already reported dropped (by upstream or by a skip)
				// is not sent, so the subscriber never receives a group it was
				// told it would not get.
				if drops.isHandled(seq) {
					d.ring.decrRef(cache)
					continue
				}

				switch d.deliverGroup(twCtx, tw, openTO, woken, cache) {
				case deliveryStopped:
					return
				case groupTimedOut:
					drops.report(moqt.SubscribeDrop{
						StartGroup: seq,
						EndGroup:   seq,
						ErrorCode:  moqt.SubscribeErrorCodeTimeout,
					})
				case groupSent:
					drops.sent(seq)
					drops.flush()
				}
				woken = false // a subsequent direct iteration means we are behind
				continue
			}
		}

		// Forward upstream drops before checking for a wakeup: a drop recorded
		// after this flush advances the notify seq, so the check below loops
		// back here instead of waiting.
		drops.flush()

		// Refresh the notification state and check if new data arrived between
		// the last iteration and now.
		state := d.notify.listen()
		if state.seq > lastState.seq {
			lastState = state
			continue
		}

		// Single wait + cancellation point. Every non-delivery path converges
		// here so the egress always observes cancellation — twCtx.Done()
		// (subscriber disconnect, or relay shutdown via conn close → stream-
		// context cancel) or d.done (upstream ended). The delivery path is
		// covered by OpenGroupAt/WriteFrame erroring on the same stream reset.
		// Without this convergence a fell-behind/cache-miss spin could blind-
		// loop past cancellation and pin gomoqt's stream-handler WaitGroup,
		// hanging Server.Shutdown/Close (#286).
		//
		// No poll-timer fallback: broadcast() (called on every fill and every
		// upstream drop) advances the notify seq and closes the previous
		// channel, waking this select; egress re-reads head() fresh each
		// iteration (level-triggered), so coalesced signals never lose data.
		// Guarded by TestRelayChain_NotifyOnlyDelivery (delivery is complete
		// and prompt with no poll fallback).
		select {
		case <-state.ch:
			// Channel closed — new data arrived. Refresh state and loop.
			lastState = d.notify.listen()
			woken = true // reached the next group via a wait (caught up)
		case <-d.done:
			// ingest records every upstream drop before closing done, so this
			// final flush forwards the last of them.
			drops.flush()
			return
		case <-twCtx.Done():
			return
		}
	}
}

// startPosition returns the ring position just before the first group a new
// subscriber is delivered: the oldest group the ring holds when fromStart,
// else the newest. Ingest runs from the moment the upstream subscription
// opens, so by the time the subscriber that opened it reaches its egress the
// ring may already hold several groups; starting it at the newest would skip
// the rest.
func (d *trackDistributor) startPosition(fromStart bool) ringPosition {
	if fromStart {
		return d.ring.earliestAvailable() - 1
	}
	last := d.ring.head()
	if last > 0 {
		last--
	}
	return last
}

// deliverResult is what deliverGroup did with one group.
type deliverResult int

const (
	// groupSent means the group was opened and every frame written.
	groupSent deliverResult = iota
	// groupTimedOut means the subscriber could not accept a new group stream
	// in time, so the group was skipped and egress continues.
	groupTimedOut
	// deliveryStopped means the subscribe stream closed, the subscriber left,
	// or the track ended; egress must return.
	deliveryStopped
)

// deliverGroup writes one group (cache) to the subscriber's track writer and
// reports what happened to it.
func (d *trackDistributor) deliverGroup(ctx context.Context, tw *moqt.TrackWriter, openTO *openTimeout, woken bool, cache *groupCache) deliverResult {
	// Stage stamps (no-op in the default build): tEnter closes the R stage
	// (group arrival → egress pickup) and opens the O stage (OpenGroupAt).
	// enterDeliver/exitDeliver bracket the concurrent-delivery gauge and the
	// deliver-span; ringResidenceSplit records R plus its fill/wake split and the
	// behind/woken classification.
	tEnter := d.stages.now()
	d.stages.enterDeliver(cache)
	defer d.stages.exitDeliver(cache, tEnter)
	d.stages.ringResidenceSplit(cache, tEnter, woken)

	// OpenGroupAt opens a new QUIC uni-stream per group. gomoqt's OpenUniStreamSync
	// blocks when the peer's MAX_STREAMS limit is reached — this is the designed
	// backpressure. openTO bounds the block with defaultGroupTimeout (reusing the
	// subscriber's timer/context instead of a per-delivery context.WithTimeout):
	// if the peer can't accept a new stream within the timeout, the group is
	// dropped (MoQ semi-reliable). Without a deadline, OpenGroupAt blocks
	// indefinitely, pinning the egress goroutine and accumulating stream objects
	// until the ring evicts everything.
	gw, err := tw.OpenGroupAt(openTO.start(), cache.seq)
	timedOut := openTO.done()
	if err != nil {
		d.ring.decrRef(cache)
		if timedOut && ctx.Err() == nil {
			// Backpressure: peer's MAX_STREAMS is exhausted. Drop this group
			// (MoQ semi-reliable) and continue to the next.
			//
			// A non-timeout failure (e.g. session death) that coincides with a
			// timer fire is also classified here; the next delivery then fails
			// fast with timedOut=false and terminates, so a dead session costs
			// at most one extra loop iteration.
			metricSubscriberSkipsTotal.Inc()
			return groupTimedOut
		}
		// Non-timeout error (connection closed, session dead): terminate egress.
		return deliveryStopped
	}
	defer gw.Close()
	defer d.ring.decrRef(cache)

	d.stages.groupOpen(tEnter)

	start := time.Now()

	// wait blocks for the next trickled frame of a still-filling group, woken by
	// the per-frame broadcast() notify (the sole wakeup — no poll fallback; see
	// the egress wait comment). It returns false on cancellation (subscriber gone
	// or distributor shut down), which ends the iteration; cancelled records that
	// so we return the right egress verdict below.
	cancelled := false
	// lastSeen tracks the notify sequence across wait() calls so a wakeup is
	// never missed. Without it, wait() would read the current channel fresh
	// inside the select: if a notify landed between frames() deciding to block
	// and wait() calling listen(), listen() would return the freshly-swapped
	// (open) channel and the select would block on it — delaying the in-flight
	// frame until the *next* notify. The seq check catches a notify that already
	// happened; selecting on the captured state.ch catches one that happens
	// after. Mirrors the seq guard in the egress loop.
	lastSeen := d.notify.listen()
	wait := func() bool {
		state := d.notify.listen()
		if state.seq > lastSeen.seq {
			lastSeen = state
			return true
		}
		select {
		case <-state.ch:
			lastSeen = d.notify.listen()
			return true
		case <-d.done:
			cancelled = true
			return false
		case <-ctx.Done():
			cancelled = true
			return false
		}
	}

	// Batch egress bytes locally per group delivery to reduce atomic CAS contention.
	var egressTotal int64
	for frame := range cache.frames(wait) {
		tWrite := d.stages.now()
		if err := gw.WriteFrame(frame); err != nil {
			// Flush bytes written before the failure: these frames were already
			// handed to QUIC, so they count against egress even
			// though the group is being abandoned. The failing frame is excluded.
			d.egressCounter.Add(float64(egressTotal))
			return deliveryStopped
		}
		d.stages.egressFrame(tWrite)
		n := int64(frame.Len())
		egressTotal += n
	}
	d.egressCounter.Add(float64(egressTotal))
	if cancelled {
		return deliveryStopped
	}

	d.deliveryHistogram.Observe(time.Since(start).Seconds())
	return groupSent
}

// subscribe and unsubscribe are removed. The broadcastNotify mechanism replaces
// per-subscriber channels with an atomic broadcast, so no per-subscriber state
// needs to be registered or cleaned up. Egress goroutines obtain (seq, ch) from
// d.notify.listen() and detect new data by comparing seq against their cached
// last-seen value. This eliminates the O(N) channel-send loop in broadcast()
// and the associated RWMutex contention.

func (d *trackDistributor) ingest(ctx context.Context, src *moqt.TrackReader) {
	defer d.manager.remove(d.trackID, d)
	defer d.sampler.removeTrack(d.trackID)

	// Upstream drops are recorded until the subscription closes; the deferred
	// shutdown below waits for this before closing done, so egress's final
	// flush sees every drop.
	var dropWg sync.WaitGroup
	dropWg.Go(func() {
		for drop := range src.Drops(ctx) {
			d.drops.record(drop)
			d.broadcast() // wakes egress, which forwards the drop
		}
	})

	// Workers must complete before egress is signalled to stop: a single
	// deferred closure enforces close(fillJobs) → fillWg.Wait() → close(done),
	// so every reserved cache is filled before any egress goroutine sees done.
	// The upstream subscription is closed too, or the publisher keeps sending
	// groups that nothing accepts. When ctx ended (the handler drained), it is
	// closed before the wait: closing cancels the groups still open upstream,
	// which ends the workers reading them; a worker on a group the publisher
	// holds open would otherwise block the wait for good. When the track ended
	// upstream, it is closed after the wait, so its last groups are read whole.
	defer func() {
		close(d.fillJobs) // signal workers to exit once in-flight jobs drain
		drained := ctx.Err() != nil
		if drained {
			_ = src.Close() // not actionable: the subscription is ending either way
		}
		d.fillWg.Wait()
		if !drained {
			_ = src.Close() // not actionable: the subscription is ending either way
		}
		dropWg.Wait()
		close(d.done)
	}()

	for {
		gr, err := src.AcceptGroup(ctx)
		if err != nil {
			return
		}
		if !d.processGroup(ctx, gr.GroupSequence(), gr) {
			return
		}
	}
}

// processGroup acquires a backpressure slot, reserves a ring slot, and
// dispatches a fill job to the worker pool. The slot is acquired BEFORE reserve
// so context cancellation never leaks a reserved cache. The send to fillJobs is
// non-blocking because fillSem limits in-flight jobs to ≤ channel capacity.
// It is separated from ingest so tests can drive it directly with a
// fakeFrameSource without needing a real *moqt.TrackReader.
// Returns false if ctx is cancelled while waiting for a backpressure slot.
func (d *trackDistributor) processGroup(ctx context.Context, seq moqt.GroupSequence, src frameSource) bool {
	tSem := d.stages.now()
	select {
	case d.fillSem <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	d.stages.fillSemWaited(tSem) // ingest backpressure: waiting for a fill slot

	// Reserve ring slot synchronously to preserve group ordering.
	cache := d.ring.reserve(seq)
	d.stages.stampArrival(cache)
	metricGroupFillsInflight.Inc()

	// Dispatch to the worker pool (guaranteed non-blocking; see fillSem).
	d.fillJobs <- fillJob{src: src, cache: cache}
	return true
}

// fillWorker is a long-lived goroutine processing fill jobs from the pool. It
// replaces the per-group go func() pattern, eliminating goroutine
// creation/destruction overhead. Workers exit when fillJobs is closed (on ingest
// return) after draining in-flight jobs.
func (d *trackDistributor) fillWorker() {
	defer d.fillWg.Done()
	for job := range d.fillJobs {
		d.ring.fill(job.src, job.cache, d.onFrame)
		metricGroupFillsInflight.Dec()
		<-d.fillSem // release backpressure slot
	}
}

// onFrame is the per-frame fill callback. Extracted as a method so the worker
// pool avoids allocating a closure per fill job.
func (d *trackDistributor) onFrame(n int) {
	if n > 0 {
		d.ingressCounter.Add(float64(n))
	}
	d.broadcast()
}

// broadcast notifies all egress goroutines that new data is available.
// It atomically advances the notify sequence and closes the previous
// notification channel, which wakes all goroutines currently waiting on it.
// This is O(1) — no per-subscriber iteration, no RWMutex contention.
func (d *trackDistributor) broadcast() {
	t := d.stages.now()
	d.notify.notify()
	d.stages.broadcastTimed(t) // confirm broadcast is O(1) / non-blocking
}
