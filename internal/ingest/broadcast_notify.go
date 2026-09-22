package ingest

import (
	"sync"
	"sync/atomic"
)

// broadcastNotify is an O(1) broadcast notification mechanism that replaces
// per-subscriber channels with an atomic sequence number and a channel-swap
// pattern. Each call to notify() atomically increments a sequence number and
// swaps in a fresh channel while closing the old one, waking all goroutines
// waiting on the previous channel. Listeners obtain both the current sequence
// and channel atomically via listen(), so there is no race between reading the
// sequence and waiting for the next event.
//
// This eliminates the former O(N) per-subscriber channel-send loop in
// trackBuffer.notify() — which ran on every pushed video/audio frame under an
// RWMutex — and the per-subscriber channel bookkeeping (subscribe/unsubscribe
// map). It is the same mechanism the relay's trackDistributor has run since
// #332, where it measured flat ~82–86 ns across 1–1000 subscribers (was O(N),
// ~32 µs at 1000).
//
// The wake cost does not vanish: closing a channel makes the runtime wake the
// parked waiters, and N waiters still cost O(N) to schedule. What changes is
// who pays it — the cost moves from the single ingest goroutine's critical
// path (it used to walk every subscriber channel per frame) to the woken
// egress goroutines themselves, which is where the relay's fan-out-drain
// attribution put it (docs/perf/bottleneck-attribution.md).
//
// init() must be called before use (typically in newTrackBuffer). As a
// defensive measure, listen() and notify() lazy-initialise if init() was
// missed, so the type is safe to zero-value.
type broadcastNotify struct {
	state atomic.Pointer[notifyState]
	mu    sync.Mutex // serialises the close-and-recreate swap
}

// notifyState is the immutable snapshot returned by listen(). Both seq and ch
// are published atomically via atomic.Pointer so listeners always see a
// consistent pair — there is no window where a listener reads a stale seq with
// a new channel or vice versa.
type notifyState struct {
	seq uint64
	ch  chan struct{} // closed when seq advances; listeners select on this
}

// init initialises the first notifyState. Must be called before any other
// method, ideally at construction time (newTrackBuffer calls it).
func (b *broadcastNotify) init() {
	b.state.Store(&notifyState{ch: make(chan struct{})})
}

// notify advances the sequence number and wakes all waiters by closing the
// previous channel and replacing it with a fresh one. The mutex serialises the
// close-and-recreate swap, which is required for correctness: without it,
// concurrent notify() calls could both Load() the same old state and both
// close(old.ch), causing a double-close panic. notify() is single-writer per
// track (the ingest goroutine) so contention is negligible.
func (b *broadcastNotify) notify() {
	b.lazyInit()
	b.mu.Lock()
	old := b.state.Load()
	n := &notifyState{seq: old.seq + 1, ch: make(chan struct{})}
	b.state.Store(n)
	close(old.ch) // wake all waiters on the previous channel
	b.mu.Unlock()
}

// listen returns the current notifyState atomically: both seq and ch are from
// the same state snapshot, so there is no race between reading the sequence
// and selecting on the channel. If seq > lastSeen, the caller should process
// new data immediately without waiting on ch.
func (b *broadcastNotify) listen() notifyState {
	b.lazyInit()
	return *b.state.Load()
}

// lazyInit initialises the state if it hasn't been set yet. This allows the
// type to be safely used as a zero value without an explicit init() call.
func (b *broadcastNotify) lazyInit() {
	if b.state.Load() == nil {
		b.init()
	}
}
