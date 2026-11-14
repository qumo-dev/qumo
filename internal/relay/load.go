package relay

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/qumo-dev/qumo/internal/watermark"
)

// errFull refuses a client session while the relay is at its ceiling: it is
// unavailable here, and the client tries its next relay (load.go).
var errFull = errors.New("relay: at capacity")

const (
	// loadTick is how often the relay measures its egress rate and judges
	// whether it is full.
	loadTick = 2 * time.Second
	// egressWindow is the span the egress rate is measured over. Egress is
	// counted as each group is delivered, so a window of a few groups keeps
	// a rate that does not jump with every group.
	egressWindow = 10 * time.Second
	// fullRetryAfter is the Retry-After a refused WebTransport client gets:
	// about when the relay may have room again. A client with another relay
	// to try does not wait for it.
	fullRetryAfter = 15 * time.Second
)

// limit is one resource's watermark: the relay is full from max until it is
// back down to 90% of it, and refuses new client sessions from its ceiling,
// 10% above max. Zero max is no limit.
type limit struct {
	max  int64
	full bool
}

// ceiling is where new client sessions are refused (watermark.Ceiling).
func (l limit) ceiling() int64 {
	return watermark.Ceiling(l.max)
}

// judge updates whether the resource is full at value: full from max, room
// again only at or below 90% of it, so that a value hovering at max does not
// make the relay full and not full on every tick.
func (l *limit) judge(value int64) {
	switch {
	case l.max <= 0:
		l.full = false
	case value >= l.max:
		l.full = true
	case value <= watermark.Room(l.max):
		l.full = false
	}
}

// refuses reports whether value is at or above the ceiling.
func (l limit) refuses(value int64) bool {
	return l.max > 0 && value >= l.ceiling()
}

// egressSample is the relay's egress byte total at a time.
type egressSample struct {
	at    time.Time
	bytes int64
}

// loadMonitor is what the relay knows of its own load: the client sessions
// it holds and the rate it sends at, against their watermarks
// (Config.MaxClientSessions, Config.MaxEgressKbps). It decides whether the
// relay is full, exported as qumo_relay_full for a control plane to read
// (qumo-deploy's heartbeat reports it, and its placement then offers the
// relay last), and whether a new client session is refused. A relay peer is never
// refused: peering carries what clients elsewhere watch.
//
// The egress rate counts the media the relay sends to subscribers, clients
// and peers alike (statsSampler.addEgress): the line every relay shares is
// what fills. So a hub busy feeding its edges takes no more clients, which
// then go to the edges: that is what its lower watermark is for.
type loadMonitor struct {
	// clients is the client sessions admitted and not yet ended, counted
	// from admission (reserve), so that a burst of connects can't all pass
	// the ceiling before any is counted.
	clients atomic.Int64
	// sessionCeiling is where client sessions are refused; 0 is no limit.
	// It does not change after newLoadMonitor.
	sessionCeiling int64
	// egressKbps is the rate measured over the last egressWindow.
	egressKbps atomic.Int64
	// egressRefusing is set while the egress rate is at its ceiling, as of
	// the last tick. The session count is checked live (admits).
	egressRefusing atomic.Bool
	// full is set while either resource is full.
	full atomic.Bool

	// fullGauge and egressGauge are where tick exports what it judged:
	// qumo_relay_full and qumo_relay_egress_kbps.
	fullGauge   prometheus.Gauge
	egressGauge prometheus.Gauge

	mu       sync.Mutex // guards sessions, egress and samples: tick's
	sessions limit
	egress   limit
	samples  []egressSample
}

func newLoadMonitor(maxClientSessions, maxEgressKbps int64) *loadMonitor {
	sessions := limit{max: max(maxClientSessions, 0)}
	m := &loadMonitor{
		sessions:    sessions,
		egress:      limit{max: max(maxEgressKbps, 0)},
		fullGauge:   metricLoadFull,
		egressGauge: metricLoadEgressKbps,
	}
	if sessions.max > 0 {
		m.sessionCeiling = sessions.ceiling()
	}
	return m
}

// reserve takes a place for a new client session, and reports whether it
// got one: not while the egress rate is at its ceiling (as of the last
// tick), nor once the client sessions are. release gives the place back,
// once; the caller calls it when the session ends. Reserving and counting
// are one step, so concurrent connects never take the count past the
// ceiling.
func (m *loadMonitor) reserve() (release func(), ok bool) {
	if m == nil {
		return func() {}, true
	}
	if m.egressRefusing.Load() {
		return nil, false
	}
	for {
		n := m.clients.Load()
		if m.sessionCeiling > 0 && n >= m.sessionCeiling {
			return nil, false
		}
		if m.clients.CompareAndSwap(n, n+1) {
			break
		}
	}
	var once sync.Once
	return func() { once.Do(func() { m.clients.Add(-1) }) }, true
}

// run measures and judges every loadTick until ctx is done. egress reads the
// relay's egress byte total.
func (m *loadMonitor) run(ctx context.Context, egress func() int64) {
	if m == nil {
		return
	}
	ticker := time.NewTicker(loadTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.tick(now, egress())
		}
	}
}

// tick takes the egress total at now, and judges the relay from the rate
// over the window and the client sessions it holds.
func (m *loadMonitor) tick(now time.Time, egressBytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.samples = append(m.samples, egressSample{at: now, bytes: egressBytes})
	// Keep the newest sample at or before the window's start, so the rate
	// spans the whole window once there is one.
	start := 0
	for start+1 < len(m.samples) && now.Sub(m.samples[start+1].at) >= egressWindow {
		start++
	}
	m.samples = m.samples[start:]
	var kbps int64
	if first := m.samples[0]; len(m.samples) > 1 {
		if elapsed := now.Sub(first.at); elapsed > 0 {
			kbps = (egressBytes - first.bytes) * 8 * int64(time.Millisecond) / int64(elapsed)
		}
	}
	m.egressKbps.Store(kbps)

	clients := m.clients.Load()
	m.sessions.judge(clients)
	m.egress.judge(kbps)
	full := m.sessions.full || m.egress.full
	refusing := m.egress.refuses(kbps)
	if m.full.Swap(full) != full {
		slog.Info("relay: load: full changed", "full", full,
			"client_sessions", clients, "max_client_sessions", m.sessions.max,
			"egress_kbps", kbps, "max_egress_kbps", m.egress.max)
	}
	if m.egressRefusing.Swap(refusing) != refusing {
		slog.Info("relay: load: refusing new client sessions for egress changed", "refusing", refusing,
			"egress_kbps", kbps, "egress_ceiling_kbps", m.egress.ceiling())
	}
	m.fullGauge.Set(boolGauge(full))
	m.egressGauge.Set(float64(kbps))
}

// retryAfter is d as a Retry-After value: whole seconds, rounded up.
func retryAfter(d time.Duration) string {
	return strconv.FormatInt(int64((d+time.Second-1)/time.Second), 10)
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
