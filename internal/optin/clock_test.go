package optin

import (
	"context"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"

	"github.com/qumo-dev/qumo/token"
)

// fakeClock is a time that moves only when told.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

// announce returns a live announcement of path and the function that ends it.
func announce(t *testing.T, path moqt.BroadcastPath) (*moqt.Announcement, func()) {
	t.Helper()
	ann, end := moqt.NewAnnouncement(context.Background(), path)
	t.Cleanup(end)
	return ann, end
}

func TestClock(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	c := NewClock(clock.now)
	assert.Nil(t, c.Totals(), "nothing opted in yet")

	cam1, endCam1 := announce(t, "/.abr/live/cam1")
	cam2, endCam2 := announce(t, "/.abr/live/cam2")
	hls, _ := announce(t, "/.hls/live/cam1")
	plain, _ := announce(t, "/live/cam1")
	c.Track(cam1)
	c.Track(plain)
	clock.add(10 * time.Second)
	c.Track(cam2)
	c.Track(hls)
	clock.add(5 * time.Second)

	assert.Equal(t, map[token.OptIn]time.Duration{
		token.OptInABR: 20 * time.Second, // cam1 15 s, cam2 5 s
		token.OptInHLS: 5 * time.Second,
	}, c.Totals(), "live announcements count up to now; a broadcast's own announcement is not an opt-in")

	endCam1()
	clock.add(10 * time.Second)
	endCam2()
	clock.add(time.Hour)

	assert.Equal(t, map[token.OptIn]time.Duration{
		token.OptInABR: 30 * time.Second, // cam1 0–15 s, cam2 10–25 s
		token.OptInHLS: time.Hour + 15*time.Second,
	}, c.Totals(), "an ended announcement stops counting")
}

func TestClock_EndedAnnouncement(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	c := NewClock(clock.now)
	ann, end := announce(t, "/.abr/live/cam1")
	end()

	c.Track(ann)
	clock.add(time.Minute)

	assert.Nil(t, c.Totals(), "an announcement that ended before it was tracked counts nothing")
}

func TestClock_Nil(t *testing.T) {
	var c *Clock
	ann, _ := announce(t, "/.abr/live/cam1")
	c.Track(ann)
	assert.Nil(t, c.Totals(), "a nil Clock counts nothing")
}
