package relay

import (
	"context"
	"sync"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/gomoqt/transport"
)

// statsSamplerInterval is the period at which the server-wide sampler refreshes
// every connection, session, and track-depth gauge. It matches the shortest of
// the previous per-entity poll intervals (cache depth was 10s; conn/session
// were 30s). Sampling everything on the same tick is simpler and strictly more
// responsive, at negligible cost — one sweep over the registries is a handful
// of cheap gauge writes per entity.
const statsSamplerInterval = 10 * time.Second

// connStatsProvider is satisfied by native QUIC connections (quic.Connection)
// but not by WebTransport sessions. It mirrors the unexported
// probeStatsProvider interface in gomoqt/moqt/session.go.
type connStatsProvider interface {
	ConnectionStats() transport.ConnectionStats
}

// sessionStatsProvider is satisfied by *moqt.Session.
type sessionStatsProvider interface {
	Stats() moqt.SessionStats
}

// statsSampler consolidates all periodic Prometheus sampling into a single
// server-wide goroutine, replacing the previous per-connection, per-session,
// and per-track poller goroutines (pollConnStats/pollSessionStats/
// pollCacheDepth). At high fan-out — tens of thousands of concurrent sessions —
// those per-entity goroutines were a dominant GC cost: each carries a stack the
// garbage collector must scan on every cycle, and ~2 stats goroutines per
// session plus one per track added up to hundreds of thousands of stacks. A
// single sampler that sweeps registries each tick removes that stack-scan load;
// entities register on start and deregister on teardown.
//
// All methods are safe on a nil receiver so that a minimally-constructed Server
// and standalone trackDistributors (used in tests) can skip sampling without
// call-site guards.
//
// Metric-series deletion is deferred, not done inline in the deregister methods.
// The sampler writes a series with WithLabelValues(...).Set from its own
// goroutine; if a deregister called DeleteLabelValues concurrently, the sampler
// could re-create ("resurrect") the just-deleted series and leak it. So
// deregistration only removes the entry from its registry (so it stops being
// sampled) and enqueues a reap closure; reapDeleted() runs those closures on the
// sampler goroutine right after each sweep, where they cannot interleave a Set.
type statsSampler struct {
	conns    sync.Map // addr string -> connStatsProvider
	sessions sync.Map // addr string -> sessionStatsProvider
	tracks   sync.Map // trackID string -> *trackDistributor

	// reap holds label-deletion closures queued by deregistration and applied on
	// the sampler goroutine after each sweep (see reapDeleted). A mutex-guarded
	// slice rather than a channel, so deregistration never blocks and a burst of
	// teardowns can never drop a deletion.
	reapMu sync.Mutex
	reap   []func()

	// stageAgg is the server-wide stage-latency collector (see stage_latency.go).
	// Zero-size no-op in the default build; real histograms with -tags instrument.
	stageAgg stageCollector
}

// queueReap enqueues fn to run on the sampler goroutine after the next sweep.
func (s *statsSampler) queueReap(fn func()) {
	s.reapMu.Lock()
	defer s.reapMu.Unlock()
	s.reap = append(s.reap, fn)
}

// reapDeleted applies and clears all queued reap closures. It must run on the
// sampler goroutine (called from run after sample) so metric deletions never
// race the sampler's own Set writes.
func (s *statsSampler) reapDeleted() {
	if s == nil {
		return
	}
	// Detach the batch under the lock, then release it before running the
	// closures — no defer, since holding reapMu across the closures would block
	// concurrent queueReap callers for the duration of the deletions.
	s.reapMu.Lock()
	batch := s.reap
	s.reap = nil
	s.reapMu.Unlock()
	for _, fn := range batch {
		fn()
	}
}

func (s *statsSampler) addConn(addr string, p connStatsProvider) {
	if s == nil {
		return
	}
	s.conns.Store(addr, p)
}

func (s *statsSampler) removeConn(addr string) {
	if s == nil {
		return
	}
	s.conns.Delete(addr)
	s.queueReap(func() {
		// Skip if addr was re-registered before the reap ran (ephemeral-port
		// reuse): the new owner keeps the series. The Load is safe here because
		// reapDeleted runs on the sampler goroutine.
		if _, ok := s.conns.Load(addr); ok {
			return
		}
		metricConnSmoothedRTT.DeleteLabelValues(addr)
		metricConnPacketLossRate.DeleteLabelValues(addr)
	})
}

func (s *statsSampler) addSession(addr string, p sessionStatsProvider) {
	if s == nil {
		return
	}
	s.sessions.Store(addr, p)
}

func (s *statsSampler) removeSession(addr string) {
	if s == nil {
		return
	}
	s.sessions.Delete(addr)
	s.queueReap(func() {
		if _, ok := s.sessions.Load(addr); ok {
			return // re-registered before reap; new owner keeps the series
		}
		metricSessionRTTMilliseconds.DeleteLabelValues(addr)
		metricSessionEstimatedBitrate.DeleteLabelValues(addr)
		// The per-addr RTT histogram series was never cleaned up by the old
		// per-session poller (it deleted only the two gauges), leaking one series
		// per departed session. Deregistration is the right place to drop it.
		metricSessionRTTHistogram.DeleteLabelValues(addr)
	})
}

func (s *statsSampler) addTrack(trackID string, d *trackDistributor) {
	if s == nil {
		return
	}
	s.tracks.Store(trackID, d)
}

func (s *statsSampler) removeTrack(trackID string) {
	if s == nil {
		return
	}
	s.tracks.Delete(trackID)
	s.queueReap(func() {
		if _, ok := s.tracks.Load(trackID); ok {
			return // re-registered before reap; new owner keeps the series
		}
		metricBufferDepthGroups.DeleteLabelValues(trackID)
	})
}

// run drives the single sampling loop until ctx is cancelled. Each tick samples
// every registered entity, then reaps the series of entities that deregistered
// since the last tick — both on this goroutine, so deletion never races a Set.
func (s *statsSampler) run(ctx context.Context) {
	if s == nil {
		return
	}
	// Flush any deletions still queued at shutdown so departed series don't
	// linger if the process outlives this server.
	defer s.reapDeleted()
	ticker := time.NewTicker(statsSamplerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sample()
			s.reapDeleted()
		}
	}
}

// sample refreshes every registered gauge once. The registries are swept
// independently; a concurrent register/deregister is safe (sync.Map) and simply
// takes effect on the next tick.
func (s *statsSampler) sample() {
	if s == nil {
		return
	}
	s.conns.Range(func(k, v any) bool {
		sampleConnStats(v.(connStatsProvider), k.(string))
		return true
	})
	s.sessions.Range(func(k, v any) bool {
		sampleSessionStats(v.(sessionStatsProvider), k.(string))
		return true
	})
	s.tracks.Range(func(k, v any) bool {
		v.(*trackDistributor).sampleCacheDepth()
		return true
	})
}

// sampleConnStats writes one snapshot of a native-QUIC connection's stats to the
// per-addr gauges. It is the unit of work the sampler performs per conn.
func sampleConnStats(p connStatsProvider, addr string) {
	stats := p.ConnectionStats()
	if stats.SmoothedRTT > 0 {
		metricConnSmoothedRTT.WithLabelValues(addr).Set(float64(stats.SmoothedRTT.Milliseconds()))
	}
	if stats.PacketsSent > 0 {
		lossRate := float64(stats.PacketsLost) / float64(stats.PacketsSent)
		metricConnPacketLossRate.WithLabelValues(addr).Set(lossRate)
	}
}

// sampleSessionStats writes one snapshot of a MoQT session's stats (RTT and
// estimated bitrate) to the per-addr gauges and RTT histogram.
func sampleSessionStats(p sessionStatsProvider, addr string) {
	stats := p.Stats()
	if stats.RTT > 0 {
		metricSessionRTTMilliseconds.WithLabelValues(addr).Set(float64(stats.RTT.Milliseconds()))
		metricSessionRTTHistogram.WithLabelValues(addr).Observe(stats.RTT.Seconds())
	}
	if stats.EstimatedBitrate > 0 {
		metricSessionEstimatedBitrate.WithLabelValues(addr).Set(float64(stats.EstimatedBitrate))
	}
}
