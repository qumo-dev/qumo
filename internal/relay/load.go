package relay

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

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
	fullRetryAfter = "15"
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
// relay is full, which the heartbeat reports so that placement offers it
// last, and whether a new client session is refused. A relay peer is never
// refused: peering carries what clients elsewhere watch.
//
// The egress rate counts the media the relay sends to subscribers, clients
// and peers alike (statsSampler.addEgress): the line every relay shares is
// what fills.
type loadMonitor struct {
	clients atomic.Int64
	// egressKbps is the rate measured over the last egressWindow.
	egressKbps atomic.Int64
	// egressRefusing is set while the egress rate is at its ceiling, as of
	// the last tick. The session count is checked live (admits).
	egressRefusing atomic.Bool
	// full is set while either resource is full.
	full atomic.Bool

	mu       sync.Mutex // guards sessions, egress and samples, written by tick
	sessions limit
	egress   limit
	samples  []egressSample
}

func newLoadMonitor(maxClientSessions, maxEgressKbps int64) *loadMonitor {
	return &loadMonitor{
		sessions: limit{max: max(maxClientSessions, 0)},
		egress:   limit{max: max(maxEgressKbps, 0)},
	}
}

// admits reports whether a new client session is taken: not while either
// resource is at its ceiling. The session count is read now; the egress
// rate as of the last tick.
func (m *loadMonitor) admits() bool {
	if m == nil {
		return true
	}
	m.mu.Lock()
	sessions := m.sessions
	m.mu.Unlock()
	return !sessions.refuses(m.clients.Load()) && !m.egressRefusing.Load()
}

// clientStarted counts a client session the relay serves, until the
// returned func is called.
func (m *loadMonitor) clientStarted() func() {
	if m == nil {
		return func() {}
	}
	m.clients.Add(1)
	return func() { m.clients.Add(-1) }
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
	metricLoadFull.Set(boolGauge(full))
	metricLoadEgressKbps.Set(float64(kbps))
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
