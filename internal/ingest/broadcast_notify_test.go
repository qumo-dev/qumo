package ingest

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestBroadcastNotify_Init(t *testing.T) {
	var n broadcastNotify
	n.init()
	state := n.listen()
	assert.Equal(t, uint64(0), state.seq, "initial seq should be 0")
	assert.NotNil(t, state.ch, "initial channel should not be nil")
}

func TestBroadcastNotify_ZeroValueSafe(t *testing.T) {
	// Verify lazyInit makes zero-value safe
	var n broadcastNotify
	state := n.listen()
	assert.Equal(t, uint64(0), state.seq, "zero-value should return seq=0")
	assert.NotNil(t, state.ch, "zero-value should return valid channel")
}

func TestBroadcastNotify_NotifyAdvancesSeq(t *testing.T) {
	var n broadcastNotify
	n.init()

	state := n.listen()
	assert.Equal(t, uint64(0), state.seq)

	n.notify()

	state = n.listen()
	assert.Equal(t, uint64(1), state.seq, "notify should advance seq by 1")

	n.notify()
	state = n.listen()
	assert.Equal(t, uint64(2), state.seq, "second notify should advance seq to 2")
}

// TestBroadcastNotify_NoListenerNoSwap pins the C18 fast-path contract: with
// no registered listener, notify() advances the sequence but must NOT close
// the current channel (nobody is parked on it) and must not allocate — the
// swap exists only to wake waiters.
func TestBroadcastNotify_NoListenerNoSwap(t *testing.T) {
	var n broadcastNotify
	n.init()

	before := n.listen()
	n.notify()
	n.notify()

	state := n.listen()
	assert.Equal(t, uint64(2), state.seq, "seq must advance on the fast path")
	select {
	case <-state.ch:
		t.Fatal("current channel must remain open with no listeners")
	default:
	}
	select {
	case <-before.ch:
		t.Fatal("no channel may be closed when nobody is listening")
	default:
	}

	// The whole point of the fast path: a listener-less notify allocates
	// nothing (was 2 objects/128 B per push).
	assert.Equal(t, 0.0, testing.AllocsPerRun(100, func() {
		n.notify()
	}), "zero-listener notify must not allocate")
}

// TestBroadcastNotify_SwapStillWakesRegisteredListener verifies the slow path
// is intact: a registered listener parked on the captured channel is woken by
// the close.
func TestBroadcastNotify_SwapStillWakesRegisteredListener(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var n broadcastNotify
		n.init()

		state := n.addListener()
		defer n.removeListener()

		woken := make(chan struct{})
		go func() {
			<-state.ch
			close(woken)
		}()
		synctest.Wait() // let the goroutine reach the receive

		n.notify()
		synctest.Wait()

		select {
		case <-woken:
			// Expected: registered listener woken by the close.
		default:
			t.Fatal("registered listener was not woken")
		}

		fresh := n.listen()
		assert.Equal(t, state.seq+1, fresh.seq, "sequence must advance")
		select {
		case <-fresh.ch:
			t.Fatal("fresh channel must be open")
		default:
		}
	})
}

// TestBroadcastNotify_FastPathNotifyThenAddListener covers one half of the
// lost-wakeup window: a notify that took the fast path before the listener
// attached is visible to that listener via the returned sequence — it must
// never park on a channel that will not be closed for an event already fired.
func TestBroadcastNotify_FastPathNotifyThenAddListener(t *testing.T) {
	var n broadcastNotify
	n.init()

	n.notify() // fast path: seq 1, no swap

	state := n.addListener()
	defer n.removeListener()

	assert.Equal(t, uint64(1), state.seq,
		"addListener must observe the sequence already advanced by the fast-path notify")
}

// TestBroadcastNotify_AddListenerThenNotifyClosesCaptured covers the other
// half: once a listener is registered, every subsequent notify takes the slow
// path and closes exactly the channel the listener captured — regardless of
// how the registration interleaved with prior fast-path notifies.
func TestBroadcastNotify_AddListenerThenNotifyClosesCaptured(t *testing.T) {
	var n broadcastNotify
	n.init()

	n.notify() // fast path fires before anyone listens
	n.notify()

	state := n.addListener()
	defer n.removeListener()

	n.notify()

	select {
	case <-state.ch:
		// Expected: the captured channel was closed by the slow path.
	default:
		t.Fatal("registered listener's captured channel must be closed by notify")
	}
}

// TestBroadcastNotify_RemoveListenerRestoresFastPath verifies the count is
// what gates the swap: after the listener leaves, notifies stop closing
// channels again.
func TestBroadcastNotify_RemoveListenerRestoresFastPath(t *testing.T) {
	var n broadcastNotify
	n.init()

	state := n.addListener()
	n.removeListener()

	n.notify()

	assert.Equal(t, uint64(1), n.listen().seq, "seq must still advance")
	select {
	case <-state.ch:
		t.Fatal("channel must not be closed after the last listener left")
	default:
	}
}

// TestBroadcastNotify_ConcurrentRegistrationVsNotify hammers the interleaving
// the seq/lock design exists for: listeners attaching and detaching
// concurrently with fast- and slow-path notifies. No double-close may panic,
// and every attached listener's captured channel must eventually close.
func TestBroadcastNotify_ConcurrentRegistrationVsNotify(t *testing.T) {
	var n broadcastNotify
	n.init()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				n.notify()
			}
		}
	})

	const rounds = 200
	for range rounds {
		state := n.addListener()
		n.notify()
		select {
		case <-state.ch:
			// The notify's close reached our captured channel, or it closed
			// before we attached and the seq we read covers it.
		case <-time.After(time.Second):
			t.Fatal("attached listener never observed its wakeup")
		}
		n.removeListener()
	}

	close(stop)
	wg.Wait()

	n.notify()
	assert.Greater(t, n.listen().seq, uint64(rounds), "sequence must account for every notify")
}

func TestBroadcastNotify_MultipleNotify(t *testing.T) {
	var n broadcastNotify
	n.init()

	const iterations = 1000
	for range iterations {
		n.notify()
	}

	state := n.listen()
	assert.Equal(t, uint64(iterations), state.seq)
}

func TestBroadcastNotify_ListenReturnsConsistentState(t *testing.T) {
	var n broadcastNotify
	n.init()

	n.notify()
	n.notify()

	// listen() should return both seq and ch from the same coherent view
	state := n.listen()
	assert.Equal(t, uint64(2), state.seq, "seq should be 2 after two notifies")
	assert.NotNil(t, state.ch, "channel should not be nil")

	// Channel should be open (fast path never closes it)
	select {
	case <-state.ch:
		t.Error("current channel should be open")
	default:
		// Expected
	}
}

// TestBroadcastNotify_ConcurrentNotifySerialised verifies the mutex contract:
// concurrent notify() calls from multiple goroutines must not double-close a
// channel (which would panic) and must advance the sequence exactly once per
// call. In ingest, notify is single-writer per track today; this test guards
// the primitive's contract so future multi-writer callers cannot break it.
func TestBroadcastNotify_ConcurrentNotifySerialised(t *testing.T) {
	var n broadcastNotify
	n.init()

	const writers = 8
	const perWriter = 250

	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			for range perWriter {
				n.notify()
			}
		})
	}
	wg.Wait()

	state := n.listen()
	assert.Equal(t, uint64(writers*perWriter), state.seq,
		"every notify must advance the sequence exactly once")
}
