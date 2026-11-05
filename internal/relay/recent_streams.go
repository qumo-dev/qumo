package relay

import "github.com/qumo-dev/gomoqt/moqt"

// maxRecentGroupStreams is how many written group streams the relay keeps
// for one subscriber. Opening one more resets the oldest.
//
// Each stream kept costs memory even after it was delivered, since holding it
// keeps quic-go's stream object alive, so the limit is small: with 2000 audio
// subscribers on one core, 8 kept the relay below main's memory while 100
// doubled it. It is the group ring's default size (DefaultGroupCacheSize).
// At 50 audio groups a second, 8 groups are 160 ms. A stream reset that late
// has normally been delivered already, and the receiver ignores the reset.
// What it can cut is a group still waiting for a retransmission, which is
// too late to play anyway.
const maxRecentGroupStreams = 8

// groupCanceler is the part of a written group stream that recentStreams
// needs: resetting it. *moqt.GroupWriter satisfies it.
type groupCanceler interface {
	CancelWrite(code moqt.GroupErrorCode)
}

var _ groupCanceler = (*moqt.GroupWriter)(nil)

// recentStreams holds the last group streams one subscriber's egress has
// written and closed, in a fixed-size ring. Adding one to a full ring resets
// the oldest.
//
// Writing and closing a group stream return as soon as its data is in
// quic-go's send buffer, not when it is sent. The subscriber's stream grant is
// the only other bound on how many unsent streams quic-go holds for it. A
// relay short of CPU, facing a subscriber that grants many streams, can
// otherwise queue unsent streams without bound while egress believes it is
// caught up. quic-go frees a reset stream's queued data at once.
//
// quic-go does not report when a stream was delivered, so a delivered stream
// is reset too once it is the oldest. Its RESET_STREAM arrives after the FIN
// and the receiver ignores it (RFC 9000 §3.2).
//
// Only the owning egress goroutine uses a recentStreams: no locking. Each
// subscriber resets its own streams, so the resets are spread across egress
// goroutines rather than fired in one burst per group.
type recentStreams struct {
	max     int
	streams []groupCanceler
	oldest  int // index of the oldest stream once the ring is full
}

// add records a written group stream. When the ring is already full, it
// resets the oldest stream, puts the new one in its place, and reports true.
func (r *recentStreams) add(gw groupCanceler) bool {
	if len(r.streams) < r.max {
		r.streams = append(r.streams, gw)
		return false
	}
	r.streams[r.oldest].CancelWrite(moqt.ExpiredGroupErrorCode)
	r.streams[r.oldest] = gw
	r.oldest = (r.oldest + 1) % r.max
	return true
}
