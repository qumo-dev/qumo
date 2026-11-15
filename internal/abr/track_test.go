package abr

import (
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
)

func frame(b byte) *moqt.Frame {
	f := moqt.NewFrame(1)
	_, _ = f.Write([]byte{b})
	return f
}

func TestTrackGroups(t *testing.T) {
	tr := newTrack()
	assert.False(t, tr.writeFrame(frame(0)), "no group open")
	assert.True(t, tr.openGroup(5))
	assert.True(t, tr.writeFrame(frame(1)))
	assert.False(t, tr.openGroup(5), "a sequence that does not advance is refused")
	assert.False(t, tr.openGroup(4))
	assert.True(t, tr.openGroup(7), "sequences may skip, as the source's do")

	// A new subscriber starts at the newest group.
	v := tr.next(0, 0, 0)
	assert.True(t, v.found)
	assert.Equal(t, moqt.GroupSequence(7), v.seq)
	assert.False(t, v.done)
	assert.Empty(t, v.frames)

	// One that served group 5 moves on to 7 from its start.
	tr.writeFrame(frame(2))
	v = tr.next(5, 0, 0)
	assert.Equal(t, moqt.GroupSequence(7), v.seq)
	assert.Len(t, v.frames, 1)

	// Serving group 5 resumes where it left off, and it is done.
	v = tr.next(0, 5, 1)
	assert.Equal(t, moqt.GroupSequence(5), v.seq)
	assert.True(t, v.done)
	assert.Empty(t, v.frames)

	tr.endGroup()
	v = tr.next(0, 7, 1)
	assert.True(t, v.done)
	assert.False(t, tr.writeFrame(frame(3)), "an ended group takes no frames")

	// Nothing after 7 yet.
	v = tr.next(7, 0, 0)
	assert.False(t, v.found)
	assert.False(t, v.closed)
}

func TestTrackRing(t *testing.T) {
	tr := newTrack()
	for seq := range moqt.GroupSequence(ringSize + 2) {
		tr.openGroup(seq + 1)
	}
	// Groups 1 and 2 left the ring: a subscriber after 1 skips to 3.
	v := tr.next(1, 0, 0)
	assert.Equal(t, moqt.GroupSequence(3), v.seq)

	// A group being served that left the ring is ended where it is.
	v = tr.next(0, 2, 4)
	assert.True(t, v.found)
	assert.True(t, v.done)
	assert.Equal(t, moqt.GroupSequence(2), v.seq)
	assert.Empty(t, v.frames)
}

func TestTrackChangedAndClosed(t *testing.T) {
	tr := newTrack()
	v := tr.next(0, 0, 0)
	assert.False(t, v.found)
	select {
	case <-v.changed:
		t.Fatal("changed before any change")
	default:
	}
	tr.openGroup(1)
	select {
	case <-v.changed:
	default:
		t.Fatal("not woken by a new group")
	}

	tr.close()
	v = tr.next(1, 0, 0)
	assert.True(t, v.closed)
	assert.False(t, v.found)
	assert.False(t, tr.openGroup(2), "a closed track takes no groups")
	tr.close() // idempotent
}
