package relay

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPendingCount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var c pendingCount
		assert.True(t, c.wait(time.Millisecond), "nothing pending")

		c.add()
		c.add()
		assert.False(t, c.wait(10*time.Millisecond), "two pending")

		go func() {
			time.Sleep(20 * time.Millisecond)
			c.done()
			c.done()
		}()
		assert.True(t, c.wait(time.Second), "both finished")

		c.add() // counting starts again after reaching zero
		assert.False(t, c.wait(10*time.Millisecond))
		c.done()
		assert.True(t, c.wait(time.Millisecond))
	})
}
