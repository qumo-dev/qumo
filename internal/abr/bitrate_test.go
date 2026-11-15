package abr

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBitrateMeter(t *testing.T) {
	m := newBitrateMeter()
	// Groups of one second; the second is the largest, at 2 Mbit/s.
	sizes := []int{125_000, 250_000, 200_000}
	var ts uint64 = 7_000_000
	for _, size := range sizes {
		for i := range 25 {
			_, ok := m.add(ts+uint64(i)*40_000, size/25, i == 0)
			assert.False(t, ok)
		}
		ts += 1_000_000
	}
	bps, ok := m.add(ts, 1000, true)
	assert.True(t, ok)
	assert.Equal(t, int64(2_000_000), bps)

	_, ok = m.add(ts+1_000_000, 1000, true)
	assert.False(t, ok, "stated once")
}
