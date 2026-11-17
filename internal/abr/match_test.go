package abr

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMatcher(t *testing.T) {
	var m matcher
	for i, ts := range []uint64{1_000_400, 1_033_733, 1_067_066, 1_100_399} {
		m.push(ts, frameInfo{Group: 1, First: i == 0, Timestamp: 1_700_000_000_000_000 + ts})
	}

	got, ok := m.match(1000) // 1_000_400 rounded to ms
	assert.True(t, ok)
	assert.Equal(t, uint64(1_700_000_001_000_400), got.Timestamp, "matched by the timestamp fed, returning the source's")
	assert.True(t, got.First)

	// The encoder dropped 1_033_733: the next output matches the frame after.
	got, ok = m.match(1067)
	assert.True(t, ok)
	assert.Equal(t, uint64(1_700_000_001_067_066), got.Timestamp)

	// An output earlier than every remembered frame matches none, and
	// forgets none.
	_, ok = m.match(900)
	assert.False(t, ok)
	got, ok = m.match(1100)
	assert.True(t, ok)
	assert.Equal(t, uint64(1_700_000_001_100_399), got.Timestamp)

	_, ok = m.match(1133)
	assert.False(t, ok, "nothing left to match")
}

func TestMatcherBounded(t *testing.T) {
	var m matcher
	for i := range maxPending + 10 {
		m.push(uint64(i)*33_333, frameInfo{})
	}
	assert.Len(t, m.pending, maxPending)
	assert.Equal(t, uint64(10*33_333), m.pending[0].fed, "the oldest are forgotten")
}
