package relay

import (
	"sync"
	"sync/atomic"

	"github.com/qumo-dev/gomoqt/moqt"
)

// dropRingSize is the number of upstream drop notifications a track keeps for
// its subscribers.
const dropRingSize = 64

// dropDistributor keeps the most recent SUBSCRIBE_DROP notifications a track
// received from upstream so each subscriber can forward them. It holds only
// group ranges, never frame payloads, and is separate from groupRing.
//
// It does not wake subscribers itself: the caller of record broadcasts on the
// track's notify, which egress already waits on.
type dropDistributor struct {
	mu    sync.RWMutex
	drops []moqt.SubscribeDrop

	// pos counts every drop ever recorded. It is written under mu and also
	// read without it by head, so a subscriber can check for new drops after
	// every group without taking the lock.
	pos atomic.Uint64
}

func newDropDistributor(size int) *dropDistributor {
	if size <= 0 {
		size = dropRingSize
	}
	return &dropDistributor{drops: make([]moqt.SubscribeDrop, size)}
}

// record stores drop, overwriting the oldest entry once the buffer is full.
func (d *dropDistributor) record(drop moqt.SubscribeDrop) {
	d.mu.Lock()
	defer d.mu.Unlock()

	pos := d.pos.Load() + 1
	d.drops[(pos-1)%uint64(len(d.drops))] = drop
	d.pos.Store(pos)
}

// head returns the number of drops recorded so far.
func (d *dropDistributor) head() uint64 {
	return d.pos.Load()
}

// since appends to buf the drops recorded after position lastPos and returns
// buf, the position to pass next time, and how many of those drops were
// overwritten before this call could read them.
func (d *dropDistributor) since(lastPos uint64, buf []moqt.SubscribeDrop) ([]moqt.SubscribeDrop, uint64, uint64) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	head := d.pos.Load()
	if head <= lastPos {
		return buf, lastPos, 0
	}

	size := uint64(len(d.drops))
	start := lastPos + 1
	var lost uint64
	if head > size && start <= head-size {
		oldest := head - size + 1
		lost = oldest - start
		start = oldest
	}

	for pos := start; pos <= head; pos++ {
		buf = append(buf, d.drops[(pos-1)%size])
	}
	return buf, head, lost
}
