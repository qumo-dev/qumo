package relay

import "github.com/qumo-dev/gomoqt/moqt"

// groupCanceler is the part of a written group stream that sentGroups needs:
// resetting it. *moqt.GroupWriter satisfies it.
type groupCanceler interface {
	CancelWrite(code moqt.GroupErrorCode)
}

var _ groupCanceler = (*moqt.GroupWriter)(nil)

// sentGroup is one group stream a subscriber's egress has written and closed.
type sentGroup struct {
	pos ringPosition
	gw  groupCanceler
}

// sentGroups holds the group streams one subscriber's egress has written and
// closed, oldest first, until the ring evicts their groups.
//
// Writing and closing a group stream return as soon as its data is in
// quic-go's send buffer, not when it is sent. A relay that is short of CPU, or
// a slow subscriber that grants many streams, can therefore queue an unbounded
// number of unsent streams in quic-go while egress believes it is caught up,
// so the ring's skip never engages. Resetting a stream once its group has left
// the ring applies the ring's rule to them: a group no longer worth holding is
// no longer worth sending. quic-go frees a reset stream's queued data at once.
//
// quic-go does not report when a stream has been delivered, so streams that
// were already delivered are reset too. Their RESET_STREAM arrives after the
// FIN and the receiver ignores it (RFC 9000 §3.2).
//
// Only the owning egress goroutine uses a sentGroups: no locking.
type sentGroups struct {
	groups []sentGroup
}

// add records a group stream written for the group at pos. Positions must be
// added in increasing order, as egress walks the ring.
func (s *sentGroups) add(pos ringPosition, gw groupCanceler) {
	s.groups = append(s.groups, sentGroup{pos: pos, gw: gw})
}

// resetBefore resets every recorded stream whose group is older than
// earliest, the oldest position the ring still holds, forgets them, and
// returns how many it reset.
func (s *sentGroups) resetBefore(earliest ringPosition) int {
	n := 0
	for n < len(s.groups) && s.groups[n].pos < earliest {
		s.groups[n].gw.CancelWrite(moqt.ExpiredGroupErrorCode)
		n++
	}
	if n == 0 {
		return 0
	}
	// Compact in place, reusing the array: the list stays within the ring's
	// size. Clearing the tail drops the references to the reset streams.
	kept := copy(s.groups, s.groups[n:])
	clear(s.groups[kept:])
	s.groups = s.groups[:kept]
	return n
}
