package relay

import (
	"sync"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/qumo-dev/qumo/internal/optin"
	"github.com/qumo-dev/qumo/token"
)

// optInClock adds up how long a session keeps broadcasts opted into each
// egress: how long each announcement of its beneath an opt-in directory
// (/.abr/live/cam) lasts, summed over its announcements, so two broadcasts
// opted into ABR for a minute each are two minutes. The session's usage
// reports carry it (auth.Request.OptIns), for a control plane to bill an
// opt-in by the time it was used. A nil clock counts nothing.
//
// It counts what the relay received from the session: an announcement the
// session's grant does not cover never reaches it, so an opt-in the
// session's key does not hold is never counted.
type optInClock struct {
	mu      sync.Mutex
	tallies map[token.OptIn]*optInTally
}

// optInTally is one egress's time: done, from announcements that ended,
// plus live announcements since since, the last time live changed.
type optInTally struct {
	done  time.Duration
	live  int
	since time.Time
}

// track counts ann, an announcement the session made, until it ends, when it
// opts a broadcast into an egress; any other is not counted, nor one that
// ended already (moqt runs AfterFunc for it later, on another goroutine,
// which would count the time until then).
func (c *optInClock) track(ann *moqt.Announcement) {
	if c == nil || !ann.IsActive() {
		return
	}
	o, ok := optin.Of(ann.BroadcastPath())
	if !ok {
		return
	}
	c.change(o, +1)
	ann.AfterFunc(func() { c.change(o, -1) })
}

// change adds delta to o's live announcements, counting the time they were
// live until now.
func (c *optInClock) change(o token.OptIn, delta int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.tallies == nil {
		c.tallies = map[token.OptIn]*optInTally{}
	}
	t := c.tallies[o]
	if t == nil {
		t = &optInTally{}
		c.tallies[o] = t
	}
	t.done += time.Duration(t.live) * now.Sub(t.since)
	t.live += delta
	t.since = now
}

// totals returns how long the session has kept broadcasts opted into each
// egress so far, live announcements counted up to now, or nil for none.
func (c *optInClock) totals() map[token.OptIn]time.Duration {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.tallies) == 0 {
		return nil
	}
	now := time.Now()
	out := make(map[token.OptIn]time.Duration, len(c.tallies))
	for o, t := range c.tallies {
		out[o] = t.done + time.Duration(t.live)*now.Sub(t.since)
	}
	return out
}
