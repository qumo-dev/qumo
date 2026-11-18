package auth

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/qumo-dev/qumo/internal/meter"
	"github.com/qumo-dev/qumo/internal/reqsign"
	"github.com/qumo-dev/qumo/token"
	"github.com/qumo-dev/qumo/usage"
)

const (
	// sessionGrace is how long a session is remembered after it was last
	// heard of, at connect or a revalidate, for an end that arrives late.
	sessionGrace = 5 * time.Minute
	// forgetInterval is how often sessions whose end never came are looked
	// for.
	forgetInterval = 10 * time.Second
)

// usageSession is what the reporter remembers of a verified session.
type usageSession struct {
	kid string
	// publishes is a session that may publish a broadcast, reported by
	// itself; every other session is a viewer, in its key's viewer total.
	publishes bool
	// seen is when the session was last heard of, at connect or a
	// revalidate: one whose end never came is forgotten once it stops being
	// revalidated.
	seen time.Time
	// counted is how much of a viewer session's bytes its key's viewer total
	// already holds. A viewer that became a publisher keeps it, and its
	// usage records carry only what it moved beyond it.
	counted Bytes
}

// usageReporter reports the relay's sessions to a usage URL
// (QUMO_USAGE_URL), through a meter.Reporter.
//
// A session that may publish is reported by itself: an open when it is
// admitted, its cumulative bytes at each revalidate, and a close with the
// final totals. Every other session is a viewer, added up per key into one
// running total for this run of the relay, sent as a viewers record. No open
// or close is sent for viewers.
type usageReporter struct {
	meter *meter.Reporter
	now   func() time.Time

	mu       sync.Mutex
	sessions map[string]usageSession
	viewers  map[string]Bytes // every viewer session's bytes this run, by kid
	// closed is when each session that ended was closed, until it is
	// forgotten as a stale one would be: an open that comes after its
	// close, from a ticket granted as the session ended, is ignored.
	closed map[string]time.Time
}

func newUsageReporter(rawURL, bearer string, signer *reqsign.Signer) (*usageReporter, error) {
	m, err := meter.New(meter.Config{Name: "relay", URL: rawURL, Token: bearer, Signer: signer})
	if err != nil {
		return nil, err
	}
	return &usageReporter{
		meter:    m,
		now:      time.Now,
		sessions: map[string]usageSession{},
		viewers:  map[string]Bytes{},
		closed:   map[string]time.Time{},
	}, nil
}

// open records a session admitted at connect, or bound to a key by the first
// ticket granted on it. One that was closed already is not brought back.
func (r *usageReporter) open(id string, s usageSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ended := r.closed[id]; ended {
		return
	}
	s.seen = r.now()
	r.sessions[id] = s
	if s.publishes {
		r.meter.Open(id, s.kid)
	}
}

// publishing records that a viewer session may now publish, such as by a
// ticket it presented: it is reported by itself from now on, opening with a
// session_open. What it moved as a viewer up to its last revalidate stays in
// its key's viewer total; what it moved since is reported as its usage. A
// session it doesn't know, or that publishes already, is left as it is.
func (r *usageReporter) publishing(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok || s.publishes {
		return
	}
	s.publishes = true
	r.sessions[id] = s
	r.meter.Open(id, s.kid)
}

// revalidated records a session's cumulative bytes and opt-in time at a
// revalidate and, when live is set, that the session is still live. A
// session it doesn't know is left unknown.
func (r *usageReporter) revalidated(id string, b Bytes, optIns map[token.OptIn]time.Duration, live bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return
	}
	if live {
		s.seen = r.now()
	}
	if !s.publishes {
		s = r.countViewer(s, b)
	} else if m := publisherMetrics(b.since(s.counted), optIns); carriesUsage(m) {
		r.meter.Report(id, s.kid, m)
	}
	r.sessions[id] = s
}

// close records a session's end with its final totals and forgets it. It
// reports false for a session it doesn't know.
func (r *usageReporter) close(id string, b Bytes, optIns map[token.OptIn]time.Duration, reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed[id] = r.now()
	s, ok := r.sessions[id]
	if !ok {
		return false
	}
	delete(r.sessions, id)
	if !s.publishes {
		r.countViewer(s, b)
		return true
	}
	r.meter.Close(id, s.kid, publisherMetrics(b.since(s.counted), optIns), reason)
	return true
}

// countViewer adds what a viewer session moved since it was last counted to
// its key's viewer total, and returns the session with that recorded. The
// caller holds mu.
func (r *usageReporter) countViewer(s usageSession, b Bytes) usageSession {
	t := r.viewers[s.kid]
	if b.Sent > s.counted.Sent {
		t.Sent += b.Sent - s.counted.Sent
		s.counted.Sent = b.Sent
	}
	if b.Received > s.counted.Received {
		t.Received += b.Received - s.counted.Received
		s.counted.Received = b.Received
	}
	r.viewers[s.kid] = t
	r.meter.Total(s.kid, usageMetrics(t))
	return s
}

func usageMetrics(b Bytes) map[string]int64 {
	return map[string]int64{usage.IngressBytes: clampInt64(b.Received), usage.EgressBytes: clampInt64(b.Sent)}
}

// publisherMetrics is a publishing session's metrics: its bytes, and how
// long it kept broadcasts opted into each egress, in whole seconds.
func publisherMetrics(b Bytes, optIns map[token.OptIn]time.Duration) map[string]int64 {
	m := usageMetrics(b)
	for o, d := range optIns {
		if secs := int64(d / time.Second); secs > 0 {
			m[usage.OptInSeconds(string(o))] = secs
		}
	}
	return m
}

// carriesUsage reports whether m holds any usage: a session that moved
// nothing and kept nothing opted in for a whole second has none to report.
func carriesUsage(m map[string]int64) bool {
	for _, v := range m {
		if v > 0 {
			return true
		}
	}
	return false
}

func clampInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// run sends usage until ctx ends, then once more, forgetting sessions whose
// end never came as it goes.
func (r *usageReporter) run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { r.meter.Run(ctx) })
	wg.Go(func() {
		ticker := time.NewTicker(forgetInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.forgetStale()
			}
		}
	})
	wg.Wait()
}

// forgetStale drops sessions whose end never came: those not heard of for
// sessionGrace.
func (r *usageReporter) forgetStale() {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := r.now().Add(-sessionGrace)
	for id, s := range r.sessions {
		if s.seen.Before(cutoff) {
			delete(r.sessions, id)
			r.meter.Forget(id)
		}
	}
	for id, at := range r.closed {
		if at.Before(cutoff) {
			delete(r.closed, id)
		}
	}
}

// since returns what b moved beyond base, each total at least zero.
func (b Bytes) since(base Bytes) Bytes {
	return Bytes{Sent: b.Sent - min(b.Sent, base.Sent), Received: b.Received - min(b.Received, base.Received)}
}
