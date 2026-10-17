package relay

import (
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDropDistributor_RecordAndSince(t *testing.T) {
	dist := newDropDistributor(4)
	require.NotNil(t, dist)

	// Initially empty
	drops, pos := dist.since(0)
	assert.Nil(t, drops)
	assert.Equal(t, uint64(0), pos)

	// Record drop 1
	d1 := moqt.SubscribeDrop{StartGroup: 1, EndGroup: 2, ErrorCode: moqt.SubscribeErrorCodeInternal}
	dist.record(d1)

	drops, pos = dist.since(0)
	assert.Equal(t, []moqt.SubscribeDrop{d1}, drops)
	assert.Equal(t, uint64(1), pos)

	// Consecutive since with updated pos returns nothing
	drops, nextPos := dist.since(pos)
	assert.Nil(t, drops)
	assert.Equal(t, pos, nextPos)

	// Record drop 2 and 3
	d2 := moqt.SubscribeDrop{StartGroup: 3, EndGroup: 4, ErrorCode: moqt.SubscribeErrorCodeInternal}
	d3 := moqt.SubscribeDrop{StartGroup: 5, EndGroup: 6, ErrorCode: moqt.SubscribeErrorCodeInternal}
	dist.record(d2)
	dist.record(d3)

	drops, pos = dist.since(pos)
	assert.Equal(t, []moqt.SubscribeDrop{d2, d3}, drops)
	assert.Equal(t, uint64(3), pos)
}

func TestDropDistributor_CircularBufferWrap(t *testing.T) {
	const size = 3
	dist := newDropDistributor(size)

	d1 := moqt.SubscribeDrop{StartGroup: 1, EndGroup: 1}
	d2 := moqt.SubscribeDrop{StartGroup: 2, EndGroup: 2}
	d3 := moqt.SubscribeDrop{StartGroup: 3, EndGroup: 3}
	d4 := moqt.SubscribeDrop{StartGroup: 4, EndGroup: 4}
	d5 := moqt.SubscribeDrop{StartGroup: 5, EndGroup: 5}

	dist.record(d1)
	dist.record(d2)
	dist.record(d3)
	dist.record(d4)
	dist.record(d5)

	// Buffer capacity is 3, so only d3, d4, d5 are retained.
	// A reader that fell behind at pos 0 is clamped to earliest retained (pos 3).
	drops, pos := dist.since(0)
	assert.Equal(t, uint64(5), pos)
	assert.Equal(t, []moqt.SubscribeDrop{d3, d4, d5}, drops)
}

func TestDropDistributor_NotificationWakeup(t *testing.T) {
	dist := newDropDistributor(4)

	state := dist.notify.listen()
	assert.NotNil(t, state.ch)

	dist.record(moqt.SubscribeDrop{StartGroup: 1, EndGroup: 2})

	select {
	case <-state.ch:
		// Notified successfully
	default:
		t.Fatal("expected notification channel to be closed after record")
	}

	nextState := dist.notify.listen()
	assert.Greater(t, nextState.seq, state.seq)
}
