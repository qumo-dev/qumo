package relay

import (
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
)

// relayMaxLatency is the longest a group's stream may stay open to a
// subscriber once the next group has arrived at the relay: the relay's own
// bound on what it buffers per subscriber. A subscriber's Max Latency can only
// shorten it. 400 ms is the one-way delay past which conversational audio
// stops being acceptable (ITU-T G.114).
const relayMaxLatency = 400 * time.Millisecond

// subscriberMaxLatency is the Max Latency egress expires a subscriber's groups
// at: the subscriber's own (requestedMs, in milliseconds as moq-lite carries
// it), capped at relayMaxLatency. A Max Latency of 0 is taken as unset and gets
// relayMaxLatency, unlike moq-lite (where 0 asks for the latest group only):
// clients send 0 when they do not set one, and honouring it as "latest only"
// reset groups still in flight to subscribers that keep up.
func subscriberMaxLatency(requestedMs uint64) time.Duration {
	if requestedMs == 0 || requestedMs > uint64(relayMaxLatency/time.Millisecond) {
		return relayMaxLatency
	}
	return time.Duration(requestedMs) * time.Millisecond
}

// maxLiveGroupStreams bounds the group streams kept for one subscriber,
// whatever its Max Latency, so a subscriber that asks for a long one cannot
// make the relay hold its streams without bound. 64 is 1.3 s of 50-a-second
// audio.
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
	arrival int64 // its group's arrival at the relay, on monoNanos's clock
}

// groupExpiry holds the group streams one subscriber's egress has opened,
// oldest first, and resets the oldest when a new one opens, while either holds:
//
//   - it is past maxLiveGroupStreams;
//   - its group has left the ring (keep, the ring's size, newer streams were
//     opened after it) and the next group arrived at the relay at least Max
//     Latency ago. The relay never resets a stream for a group it still
//     holds, so for long groups (video) the ring's window is the bound that
//     applies, and for short groups (audio) the age is.
//
// Age is relay policy, measured on the relay's monotonic clock from each
// group's arrival, not moq-lite's media time: it bounds what the relay
// buffers, needs no frame timestamps, and counts the time a group waited for
// a relay short of CPU. The latest stream never expires.
//
// Writing and closing a group stream return as soon as its data is in
// quic-go's send buffer, not when it is sent. Without expiry, a subscriber
// that grants many streams and stops reading, or a relay out of CPU, lets
// unsent streams pile up without bound while egress believes it is caught up.
// Resetting a stream makes quic-go drop its unsent data at once.
//
// quic-go does not report when a stream was delivered, so a stream that was
// already delivered is reset too. The receiver ignores a RESET_STREAM that
// arrives after the FIN.
//
// Only the owning egress goroutine uses a groupExpiry: no locking.
type groupExpiry struct {
	keep    int              // streams never reset for age: the ring's size
	streams []expiringStream // oldest first; the last is the latest group
}

// open records the stream just opened for the next group, which arrived at
// arrival and becomes the latest, then resets the streams it makes expire or
// puts past maxLiveGroupStreams, as of now (both on monoNanos's clock). It
// returns how many it reset.
func (e *groupExpiry) open(gw groupCanceler, arrival, now int64, maxLatency time.Duration) int {
	e.streams = append(e.streams, expiringStream{gw: gw, arrival: arrival})
	n := 0
	for ; n < len(e.streams)-1; n++ {
		capped := len(e.streams)-n > maxLiveGroupStreams
		// Groups arrive in ring order, so once one is young enough or still
		// in the ring, every later one is too.
		expired := len(e.streams)-1-n >= e.keep && now-e.streams[n+1].arrival >= int64(maxLatency)
		if !capped && !expired {
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

// monoEpoch is the reference monoNanos counts from. It is set once at start
// and never changed.
var monoEpoch = time.Now()

// monoNanos is the time now on the relay's monotonic clock, in nanoseconds
// since monoEpoch: wall clock changes do not move it.
func monoNanos() int64 {
	return int64(time.Since(monoEpoch))
}
