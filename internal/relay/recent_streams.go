package relay

import (
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
)

// How long a subscriber's written group streams are kept before egress resets
// them (recentStreams). A stream is reset once it is older than
// recentStreamMaxAge and at least recentStreamsMin newer streams were written
// after it, or as soon as more than recentStreamsMax are kept.
const (
	// recentStreamsMin newest streams are never reset for age, so a subscriber
	// that gets groups rarely (a paused publisher, video's long groups) keeps
	// its last few however old they are. It is the group ring's default size
	// (DefaultGroupCacheSize).
	recentStreamsMin = 8
	// recentStreamMaxAge is how long a written stream may stay unfinished. A
	// group still not delivered after it is too late to play, so resetting it
	// costs the listener nothing. Under it, a subscriber whose round trip or
	// queue is a few hundred milliseconds keeps every group: a bound by count
	// alone (8 audio groups are 160 ms) reset groups still in flight and lost
	// 0.5–3.9 % of audio at normal load.
	recentStreamMaxAge = 400 * time.Millisecond
	// recentStreamsMax bounds the streams kept per subscriber whatever their
	// age, so memory stays bounded at any group rate: 64 is 1.3 s of 50-a-second
	// audio, past recentStreamMaxAge for any track at up to 160 groups a second.
	recentStreamsMax = 64
)

// groupCanceler is the part of a written group stream that recentStreams
// needs: resetting it. *moqt.GroupWriter satisfies it.
type groupCanceler interface {
	CancelWrite(code moqt.GroupErrorCode)
}

var _ groupCanceler = (*moqt.GroupWriter)(nil)

// recentStream is a written group stream and when egress finished writing it.
type recentStream struct {
	gw      groupCanceler
	written time.Time
}

// recentStreams holds the group streams one subscriber's egress has written
// and closed, oldest first, and resets the ones it no longer keeps.
//
// Writing and closing a group stream return as soon as its data is in
// quic-go's send buffer, not when it is sent. The subscriber's stream grant is
// the only other bound on how many unsent streams quic-go holds for it, so a
// subscriber that grants many streams and then stops reading (or a relay out
// of CPU) would otherwise let unsent streams pile up without bound while
// egress believes it is caught up. Resetting a stream makes quic-go drop its
// unsent data at once.
//
// quic-go does not report when a stream was delivered, so a delivered stream
// is reset too once it is old enough, and the receiver ignores a RESET_STREAM
// that arrives after the FIN. quic-go v0.64 still sends one for a stream whose
// data and FIN were all acknowledged, although that stream is in the terminal
// "Data Recvd" state (RFC 9000, Section 3.1); that is most of what these
// resets cost.
//
// Only the owning egress goroutine uses a recentStreams: no locking.
type recentStreams struct {
	streams []recentStream // oldest first
}

// add records a stream written at now, then resets the oldest streams the
// policy no longer keeps, and returns how many it reset.
func (r *recentStreams) add(gw groupCanceler, now time.Time) int {
	r.streams = append(r.streams, recentStream{gw: gw, written: now})
	n := 0
	for kept := len(r.streams); kept > recentStreamsMin; kept-- {
		oldest := r.streams[n]
		if kept <= recentStreamsMax && now.Sub(oldest.written) < recentStreamMaxAge {
			break
		}
		oldest.gw.CancelWrite(moqt.ExpiredGroupErrorCode)
		n++
	}
	if n > 0 {
		// Compact in place: the list stays within recentStreamsMax, and
		// clearing the tail drops the references to the reset streams.
		kept := copy(r.streams, r.streams[n:])
		clear(r.streams[kept:])
		r.streams = r.streams[:kept]
	}
	return n
}
