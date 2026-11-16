package abr

import (
	"context"
	"sync"

	"github.com/qumo-dev/gomoqt/moqt"
)

// ringSize is how many groups a track keeps for subscribers. A subscriber is
// served from the newest group on, so only one that falls behind reads older
// ones, and a few seconds of video is plenty.
const ringSize = 4

// A track holds the recent groups of one published track and serves them to
// subscribers, under the group sequences its writer gives them.
//
// The sequences are what make the output broadcast switchable: every rendition
// numbers its groups as the source does, so group N of each track starts at
// the same frame, and a subscriber can leave one track after group N and join
// another at group N+1.
type track struct {
	mu      sync.Mutex
	groups  []*group      // oldest first, at most ringSize
	changed chan struct{} // closed, and replaced, on every change
	closed  bool
}

// A group is one group of a track.
type group struct {
	seq    moqt.GroupSequence
	frames []*moqt.Frame
	done   bool // no more frames will be added
}

func newTrack() *track {
	return &track{changed: make(chan struct{})}
}

// notifyLocked wakes every subscriber waiting for a change.
func (t *track) notifyLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}

// openGroup ends the current group and starts group seq. A seq that does not
// advance on the current group's is refused: subscribers are served in
// sequence order, and would never be served it.
func (t *track) openGroup(seq moqt.GroupSequence) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	if n := len(t.groups); n > 0 {
		cur := t.groups[n-1]
		if seq <= cur.seq {
			return false
		}
		cur.done = true
	}
	if len(t.groups) == ringSize {
		t.groups = append(t.groups[:0], t.groups[1:]...)
	}
	t.groups = append(t.groups, &group{seq: seq})
	t.notifyLocked()
	return true
}

// writeFrame adds f to the current group. It reports false when there is no
// group open.
func (t *track) writeFrame(f *moqt.Frame) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(t.groups)
	if t.closed || n == 0 || t.groups[n-1].done {
		return false
	}
	t.groups[n-1].frames = append(t.groups[n-1].frames, f)
	t.notifyLocked()
	return true
}

// endGroup ends the current group, so subscribers finish its stream rather
// than wait for frames that will not come.
func (t *track) endGroup() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := len(t.groups); n > 0 && !t.groups[n-1].done {
		t.groups[n-1].done = true
		t.notifyLocked()
	}
}

// close ends the track: subscribers are served what it holds, then their
// subscriptions end.
func (t *track) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if n := len(t.groups); n > 0 {
		t.groups[n-1].done = true
	}
	t.closed = true
	t.notifyLocked()
}

// view is what a subscriber is to serve next, read under the track's lock.
type view struct {
	seq     moqt.GroupSequence
	frames  []*moqt.Frame // frames from the subscriber's position on
	done    bool          // the group has no more frames to come
	found   bool          // there is a group to serve
	closed  bool          // the track has ended
	changed <-chan struct{}
}

// position is where a subscriber is in a track: the last group fully served,
// and the group being served with how many of its frames were sent. Flags
// say which are set, as any sequence, 0 included, is a group's.
type position struct {
	served   moqt.GroupSequence
	anyDone  bool // a group has been fully served
	current  moqt.GroupSequence
	serving  bool // a group is being served
	sentInto int  // frames of the current group sent
}

// next returns what to serve a subscriber at pos. A subscriber with nothing
// served yet starts at the newest group; one whose next group has left the
// ring skips to the oldest still held.
func (t *track) next(pos position) view {
	t.mu.Lock()
	defer t.mu.Unlock()
	v := view{closed: t.closed, changed: t.changed}
	if len(t.groups) == 0 {
		return v
	}
	from := pos.sentInto
	var g *group
	switch {
	case pos.serving:
		g = t.find(pos.current)
		if g == nil {
			// The group being served left the ring: end it where it is.
			v.seq, v.done, v.found = pos.current, true, true
			return v
		}
	case !pos.anyDone:
		g = t.groups[len(t.groups)-1]
		from = 0
	default:
		for _, c := range t.groups {
			if c.seq > pos.served {
				g = c
				break
			}
		}
		if g == nil {
			return v
		}
		from = 0
	}
	v.seq, v.done, v.found = g.seq, g.done, true
	if from < len(g.frames) {
		v.frames = g.frames[from:len(g.frames):len(g.frames)]
	}
	return v
}

func (t *track) find(seq moqt.GroupSequence) *group {
	for _, g := range t.groups {
		if g.seq == seq {
			return g
		}
	}
	return nil
}

// serve serves tw from the newest group on, until the subscriber leaves, ctx
// ends, or the track is closed and fully served.
func (t *track) serve(ctx context.Context, tw *moqt.TrackWriter) {
	defer tw.Close() // not actionable: the subscription is over either way
	var (
		pos position
		gw  *moqt.GroupWriter // group being served
	)
	defer func() {
		if gw != nil {
			_ = gw.Close() // not actionable: the subscriber is leaving
		}
	}()
	// finish marks the group being served as fully served.
	finish := func(seq moqt.GroupSequence) {
		gw = nil
		pos = position{served: seq, anyDone: true}
	}
	for {
		v := t.next(pos)
		if v.found {
			if gw == nil {
				var err error
				gw, err = tw.OpenGroupAt(ctx, v.seq)
				if err != nil {
					return
				}
				pos.current, pos.serving, pos.sentInto = v.seq, true, 0
			}
			for _, f := range v.frames {
				if err := gw.WriteFrame(f); err != nil {
					gw.CancelWrite(moqt.InternalGroupErrorCode)
					finish(v.seq)
					break
				}
				pos.sentInto++
			}
			if gw != nil && v.done {
				_ = gw.Close() // not actionable: the next group follows either way
				finish(v.seq)
			}
			if gw == nil {
				continue // look for the next group straight away
			}
		} else if v.closed {
			return
		}
		select {
		case <-v.changed:
		case <-ctx.Done():
			return
		case <-tw.Context().Done():
			return
		}
	}
}
