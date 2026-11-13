package relay

import (
	"math"
	"math/bits"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
)

// relayMaxLatency is the longest a group may stay in flight
// to a subscriber once a later group has presented a frame: the relay's own
// bound, since expiry protects the relay as much as the subscriber. A
// subscriber's Max Latency can only shorten it. 400 ms is the one-way delay
// past which conversational audio stops being acceptable (ITU-T G.114).
const relayMaxLatency = 400 * time.Millisecond

// subscriberMaxLatency is the Max Latency egress expires a subscriber's groups
// at: the subscriber's own (requestedMs, in milliseconds as moq-lite carries
// it), capped at relayMaxLatency. A Max Latency of 0 is
// taken as unset and gets relayMaxLatency, unlike moq-lite (where 0 asks for
// the latest group only): clients send 0 when they do not set one, and
// honouring it as "latest only" reset groups still in flight to subscribers
// that keep up.
func subscriberMaxLatency(requestedMs uint64) time.Duration {
	if requestedMs == 0 || requestedMs > uint64(relayMaxLatency/time.Millisecond) {
		return relayMaxLatency
	}
	return time.Duration(requestedMs) * time.Millisecond
}

// maxLiveGroupStreams bounds the group streams kept for one subscriber,
// whatever its Max Latency, so a subscriber that asks for a long one (or a
// publisher without frame timestamps) cannot make the relay hold its streams
// without bound. It is relay policy, not part of moq-lite: 64 is 1.3 s of
// 50-a-second audio.
const maxLiveGroupStreams = 64

// groupCanceler is the part of a written group stream that groupExpiry needs:
// resetting it. *moqt.GroupWriter satisfies it.
type groupCanceler interface {
	CancelWrite(code moqt.GroupErrorCode)
}

var _ groupCanceler = (*moqt.GroupWriter)(nil)

// expiringStream is a group stream egress has opened for one subscriber.
type expiringStream struct {
	gw      groupCanceler
	first   uint64 // its first frame's timestamp, once started
	started bool   // a frame was written on it
}

// groupExpiry holds the group streams one subscriber's egress has opened,
// oldest first, and resets a stream once both hold:
//
//   - its group has left the ring: keep (the ring's size) newer groups were
//     opened after it. The relay never resets a stream for a group it still
//     holds, so the ring's window applies to streams as it does to groups, and
//     for long groups (video) it is the bound that applies;
//   - its group is expired as moq-lite (draft-lcurley-moq-lite-06, Section
//     6.2) asks of a publisher: the newest frame timestamp of the latest group
//     is at least Max Latency past the first frame timestamp of the next
//     group. For short groups (audio) this is the bound that applies.
//
// The latest group never expires. Egress passes subscriberMaxLatency, not the
// subscriber's raw value.
//
// Writing and closing a group stream return as soon as its data is in
// quic-go's send buffer, not when it is sent. Without expiry, a subscriber
// that grants many streams and stops reading, or a relay out of CPU, lets
// unsent streams pile up without bound while egress believes it is caught up.
// Resetting an expired stream makes quic-go drop its unsent data at once.
//
// quic-go does not report when a stream was delivered, so an expired stream
// that was already delivered is reset too. The receiver ignores a
// RESET_STREAM that arrives after the FIN.
//
// Only the owning egress goroutine uses a groupExpiry: no locking.
type groupExpiry struct {
	keep    int              // streams never reset for age: the ring's size
	streams []expiringStream // oldest first; the last is the latest group
	latest  uint64           // newest frame timestamp written on the latest group
}

// open records the stream just opened for the next group, which becomes the
// latest, and resets the oldest streams past maxLiveGroupStreams. It returns
// how many it reset.
func (e *groupExpiry) open(gw groupCanceler) int {
	e.streams = append(e.streams, expiringStream{gw: gw})
	n := 0
	for len(e.streams)-n > maxLiveGroupStreams {
		e.streams[n].gw.CancelWrite(moqt.ExpiredGroupErrorCode)
		n++
	}
	e.forget(n)
	return n
}

// frame records a frame with timestamp ts written on the latest group's
// stream, then resets the groups it expires under maxAge (in timestamp units)
// and returns how many it reset.
func (e *groupExpiry) frame(ts, maxAge uint64) int {
	cur := &e.streams[len(e.streams)-1]
	if !cur.started {
		cur.first, cur.started = ts, true
		e.latest = ts
	} else {
		e.latest = max(e.latest, ts)
	}
	n := 0
	for ; n < len(e.streams)-1; n++ {
		if len(e.streams)-1-n < e.keep {
			break // the group is still in the ring
		}
		next := e.streams[n+1]
		// A group expires only once a later group has presented a frame, and
		// its age is measured to that frame (its reach). Timestamps that run
		// backwards count as age 0, which only a Max Latency of 0 expires.
		if !next.started {
			break
		}
		var age uint64
		if e.latest > next.first {
			age = e.latest - next.first
		}
		if age < maxAge {
			break
		}
		e.streams[n].gw.CancelWrite(moqt.ExpiredGroupErrorCode)
	}
	e.forget(n)
	return n
}

// forget drops the n oldest streams, reusing the array and clearing its tail
// so it keeps no reference to them.
func (e *groupExpiry) forget(n int) {
	if n == 0 {
		return
	}
	kept := copy(e.streams, e.streams[n:])
	clear(e.streams[kept:])
	e.streams = e.streams[:kept]
}

// maxAgeUnits converts a Max Latency to frame timestamp units at timescale
// (units per second; 0 is moqt.DefaultTimescale), saturating rather than
// overflowing. A negative Max Latency is 0.
func maxAgeUnits(maxLatency time.Duration, timescale uint64) uint64 {
	if maxLatency <= 0 {
		return 0
	}
	if timescale == 0 {
		timescale = moqt.DefaultTimescale
	}
	hi, lo := bits.Mul64(uint64(maxLatency), timescale)
	if hi >= uint64(time.Second) {
		return math.MaxUint64
	}
	q, _ := bits.Div64(hi, lo, uint64(time.Second))
	return q
}
