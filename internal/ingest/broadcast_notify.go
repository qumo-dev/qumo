package ingest

import (
	"sync"
	"sync/atomic"
)

// broadcastNotify is an O(1) broadcast notification mechanism: an atomic
// sequence number plus a close-and-recreate channel swap. Listeners obtain
// both the current sequence and channel atomically via [addListener] (or the
// read-only [listen]), and egress goroutines park on the captured channel,
// guarded by the sequence.
//
// There are two notify paths:
//
//   - Zero listeners (no egress goroutine is attached to the track): the
//     sequence advances with a single atomic add — no mutex, no allocation,
//     no channel close. A channel nobody is parked on has nobody to wake.
//   - One or more listeners: the sequence advances, the previous channel is
//     closed (waking every goroutine parked on it), and a fresh channel is
//     installed. The mutex serialises the close-and-recreate swap, which is
//     required for correctness: concurrent swaps could both close the same
//     channel.
//
// This eliminates the former O(N) per-subscriber channel-send loop in
// trackBuffer.notify() — which ran on every pushed video/audio frame under an
// RWMutex — and the per-subscriber channel bookkeeping (subscribe/unsubscribe
// map). It is the same mechanism the relay's trackDistributor has run since
// #332, where it measured flat ~82–86 ns across 1–1000 subscribers (was O(N),
// ~32 µs at 1000). The zero-listener fast path is specific to ingest, where
// most tracks spend most of their life with no attached egress.
//
// The wake cost does not vanish: closing a channel makes the runtime wake the
// parked waiters, and N waiters still cost O(N) to schedule. What changes is
// who pays it — the cost moves from the single ingest goroutine's critical
// path (it used to walk every subscriber channel per frame) to the woken
// egress goroutines themselves, which is where the relay's fan-out-drain
// attribution put it (docs/perf/bottleneck-attribution.md).
//
// init() must be called before use (newTrackBuffer does). As a defensive
// measure, all methods lazy-initialise if init() was missed, so the type is
// safe to zero-value.
type broadcastNotify struct {
	// seq is the authoritative notification sequence. It lives outside the
	// state snapshot so notify() can advance it without allocating when no
	// listener needs waking.
	seq atomic.Uint64
	// ch is the current open channel. Closed — and replaced — only by the
	// slow notify path.
	ch atomic.Pointer[chan struct{}]
	// listeners counts registered egress waiters (one per live serve() call).
	listeners atomic.Int64

	mu sync.Mutex // serialises the close-and-recreate swap
}

// notifyState is the consistent {seq, ch} snapshot returned to listeners. Both
// fields come from the same critical section, so there is no window where a
// listener reads a stale seq with a new channel or vice versa.
type notifyState struct {
	seq uint64
	ch  chan struct{} // closed when seq advances; listeners select on this
}

// init initialises the first notification channel. Must be called before any
// other method, ideally at construction time (newTrackBuffer calls it).
func (b *broadcastNotify) init() {
	ch := make(chan struct{})
	b.ch.Store(&ch)
}

// notify advances the sequence number. When listeners are registered it also
// wakes them by closing the previous channel and installing a fresh one; with
// none, it stops at the sequence bump — there is nobody to wake.
func (b *broadcastNotify) notify() {
	b.lazyInit()
	if b.listeners.Load() == 0 {
		b.seq.Add(1) // fast path: no waiters, nothing to close or allocate
		return
	}

	b.mu.Lock()
	// Re-check under the lock: a listener may have attached since the
	// unlocked check. Registration takes this mutex, so either we see it
	// here, or it observed the sequence state after our bump.
	if b.listeners.Load() == 0 {
		b.seq.Add(1)
		b.mu.Unlock()
		return
	}
	b.seq.Add(1)
	old := *b.ch.Load()
	ch := make(chan struct{})
	b.ch.Store(&ch)
	close(old) // wake all waiters on the previous channel
	b.mu.Unlock()
}

// addListener registers an egress waiter for the lifetime of one serve() call
// and returns the current state atomically with the registration: any notify()
// that starts after addListener returns either sees the listener (slow path,
// closes the captured channel) or already happened (the returned seq shows it,
// so the caller never parks on a channel that will not be closed).
func (b *broadcastNotify) addListener() notifyState {
	b.lazyInit()
	b.mu.Lock()
	b.listeners.Add(1)
	seq := b.seq.Load()
	ch := *b.ch.Load()
	b.mu.Unlock()
	return notifyState{seq: seq, ch: ch}
}

// removeListener unregisters an egress waiter. Must be called exactly once
// per addListener, when the serve() call ends.
func (b *broadcastNotify) removeListener() {
	b.listeners.Add(-1)
}

// listen returns the current {seq, ch} pair without registering. Egress
// goroutines call this to re-read state after a wake; it is safe at any
// frequency. If seq > lastSeen, the caller should process new data
// immediately without waiting on ch.
func (b *broadcastNotify) listen() notifyState {
	b.lazyInit()
	return notifyState{seq: b.seq.Load(), ch: *b.ch.Load()}
}

// lazyInit initialises the state if it hasn't been set yet. This allows the
// type to be safely used as a zero value without an explicit init() call.
func (b *broadcastNotify) lazyInit() {
	if b.ch.Load() == nil {
		ch := make(chan struct{})
		b.ch.CompareAndSwap(nil, &ch)
	}
}
