package optin

import (
	"sync"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/qumo-dev/qumo/token"
	"github.com/qumo-dev/qumo/usage"
)

// A Clock adds up how long a publisher keeps broadcasts opted into each
// egress: how long each of its announcements beneath an opt-in directory
// (/.abr/live/cam) lasts, summed over them, so two broadcasts opted into ABR
// for a minute each are two minutes. A server's usage reports carry it
// (usage.OptInSeconds), for a control plane to bill an opt-in by the time it
// was used. The zero value is ready to use, and a nil Clock counts nothing.
// Its methods are safe for concurrent use.
type Clock struct {
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

// Track counts ann, an announcement the publisher made, until it ends, when it
// opts a broadcast into an egress; any other is not counted, nor one that
// ended already (moqt runs AfterFunc for it later, on another goroutine,
// which would count the time until then).
func (c *Clock) Track(ann *moqt.Announcement) {
	if c == nil || !ann.IsActive() {
		return
	}
	o, ok := Of(ann.BroadcastPath())
	if !ok {
		return
	}
	c.change(o, +1)
	ann.AfterFunc(func() { c.change(o, -1) })
}

// change adds delta to o's live announcements, counting the time they were
// live until now.
func (c *Clock) change(o token.OptIn, delta int) {
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

// AddSeconds adds each egress's time in optIns to m, as Totals returns it,
// in whole seconds under usage.OptInSeconds. An egress with less than a
// second is left out, so a session that opted nothing in carries no metric.
func AddSeconds(m map[string]int64, optIns map[token.OptIn]time.Duration) {
	for o, d := range optIns {
		if secs := int64(d / time.Second); secs > 0 {
			m[usage.OptInSeconds(string(o))] = secs
		}
	}
}

// Totals returns how long the publisher has kept broadcasts opted into each
// egress so far, live announcements counted up to now, or nil for none.
func (c *Clock) Totals() map[token.OptIn]time.Duration {
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
