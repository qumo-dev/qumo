package relay

import (
	"sync"
	"sync/atomic"

	"github.com/qumo-dev/gomoqt/moqt"
)

// DefaultDropRingSize is the number of drop notifications retained in the
// distributor's circular buffer for subscribers.
const DefaultDropRingSize = 64

// dropDistributor is a dedicated, standalone circular buffer and broadcast
// mechanism for MoQT SUBSCRIBE_DROP control messages. It is decoupled from
// groupRing and frame delivery: drops carry only metadata ranges on the
// bidirectional control stream, with no frame payloads or pool allocations.
type dropDistributor struct {
	mu     sync.RWMutex
	drops  []moqt.SubscribeDrop
	size   int
	head   atomic.Uint64
	notify broadcastNotify
}

func newDropDistributor(size int) *dropDistributor {
	if size <= 0 {
		size = DefaultDropRingSize
	}
	d := &dropDistributor{
		drops: make([]moqt.SubscribeDrop, size),
		size:  size,
	}
	d.notify.init()
	return d
}

// record appends a drop notification to the circular buffer and notifies
// all listening subscriber egress routines.
func (d *dropDistributor) record(drop moqt.SubscribeDrop) {
	d.mu.Lock()
	pos := d.head.Add(1)
	idx := int((pos - 1) % uint64(d.size))
	d.drops[idx] = drop
	d.mu.Unlock()

	d.notify.notify()
}

// since returns any drops recorded with sequence > lastPos, up to the current
// head, along with the updated head sequence.
func (d *dropDistributor) since(lastPos uint64) ([]moqt.SubscribeDrop, uint64) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	currentHead := d.head.Load()
	if currentHead <= lastPos {
		return nil, lastPos
	}

	// If the subscriber has fallen behind more than the buffer capacity,
	// clamp to the oldest available drop in the buffer.
	earliest := uint64(1)
	if currentHead > uint64(d.size) {
		earliest = currentHead - uint64(d.size) + 1
	}
	startPos := lastPos + 1
	if startPos < earliest {
		startPos = earliest
	}

	count := int(currentHead - startPos + 1)
	if count <= 0 {
		return nil, currentHead
	}

	res := make([]moqt.SubscribeDrop, count)
	for i := range count {
		pos := startPos + uint64(i)
		idx := int((pos - 1) % uint64(d.size))
		res[i] = d.drops[idx]
	}
	return res, currentHead
}
