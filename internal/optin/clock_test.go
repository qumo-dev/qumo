package optin

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"

	"github.com/qumo-dev/qumo/token"
)

// announce returns a live announcement of path and the function that ends it.
func announce(t *testing.T, path moqt.BroadcastPath) (*moqt.Announcement, func()) {
	t.Helper()
	ann, end := moqt.NewAnnouncement(context.Background(), path)
	t.Cleanup(end)
	return ann, end
}

func TestClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := NewClock()
		assert.Nil(t, c.Totals(), "nothing opted in yet")

		cam1, endCam1 := announce(t, "/.abr/live/cam1")
		cam2, endCam2 := announce(t, "/.abr/live/cam2")
		hls, _ := announce(t, "/.hls/live/cam1")
		plain, _ := announce(t, "/live/cam1")
		c.Track(cam1)
		c.Track(plain)
		time.Sleep(10 * time.Second)
		c.Track(cam2)
		c.Track(hls)
		time.Sleep(5 * time.Second)

		assert.Equal(t, map[token.OptIn]time.Duration{
			token.OptInABR: 20 * time.Second, // cam1 15 s, cam2 5 s
			token.OptInHLS: 5 * time.Second,
		}, c.Totals(), "live announcements count up to now; a broadcast's own announcement is not an opt-in")

		endCam1()
		time.Sleep(10 * time.Second)
		endCam2()
		time.Sleep(time.Hour)

		assert.Equal(t, map[token.OptIn]time.Duration{
			token.OptInABR: 30 * time.Second, // cam1 0–15 s, cam2 10–25 s
			token.OptInHLS: time.Hour + 15*time.Second,
		}, c.Totals(), "an ended announcement stops counting")
	})
}

func TestClock_EndedAnnouncement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := NewClock()
		ann, end := announce(t, "/.abr/live/cam1")
		end()

		c.Track(ann)
		time.Sleep(time.Minute)

		assert.Nil(t, c.Totals(), "an announcement that ended before it was tracked counts nothing")
	})
}

func TestClock_Nil(t *testing.T) {
	var c *Clock
	ann, _ := announce(t, "/.abr/live/cam1")
	c.Track(ann)
	assert.Nil(t, c.Totals(), "a nil Clock counts nothing")
}
