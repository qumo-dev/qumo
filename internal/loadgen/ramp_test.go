package loadgen

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestLaunchSubscribers_Burst pins the default: ramp 0 launches every session
// immediately (no pacing window), preserving the historical burst behavior.
func TestLaunchSubscribers_Burst(t *testing.T) {
	var mu sync.Mutex
	times := make([]time.Time, 0, 50)
	var wg sync.WaitGroup
	launches := launchSubscribers(context.Background(), &wg, 50, 0, func() {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
	})
	wg.Wait()
	assert.Equal(t, 50, launches)
	assert.Len(t, times, 50)
}

// TestLaunchSubscribers_RampSpacing verifies the pacing contract: at R
// sessions/second the i-th session's first dial is scheduled at i/R seconds,
// so 10 sessions at 200/s span >= 45ms of pacing (burst would span ~0). The
// upper bound keeps a pathological stall from passing silently.
func TestLaunchSubscribers_RampSpacing(t *testing.T) {
	var mu sync.Mutex
	times := make([]time.Time, 0, 10)
	var wg sync.WaitGroup
	start := time.Now()
	launches := launchSubscribers(context.Background(), &wg, 10, 200, func() {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
	})
	wg.Wait()
	elapsed := time.Since(start)
	assert.Equal(t, 10, launches)
	assert.Len(t, times, 10)
	assert.GreaterOrEqual(t, elapsed, 40*time.Millisecond,
		"10 sessions at 200/s require >=45ms of pacing")
	assert.Less(t, elapsed, 2*time.Second, "pacing must not stall")
}

// TestLaunchSubscribers_CancelMidRamp verifies the abort path: cancelling ctx
// mid-ramp stops further launches (so a Ctrl-C during a long ramp cannot leave
// an unbounded wait) and returns the count launched so far.
func TestLaunchSubscribers_CancelMidRamp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	var ran atomic.Int32
	done := make(chan int, 1)
	go func() {
		done <- launchSubscribers(ctx, &wg, 100, 1, func() { ran.Add(1) })
	}()
	// i=0 launches immediately; i=1's slot is 1s away — cancel well before.
	time.Sleep(300 * time.Millisecond)
	cancel()
	got := <-done
	wg.Wait()
	assert.Equal(t, 1, got, "only the first session launches before cancel")
	assert.Equal(t, int32(1), ran.Load())
}

// TestRunSubscribe_NegativeRamp validates the flag surface: a negative ramp is
// rejected before any dialing happens.
func TestRunSubscribe_NegativeRamp(t *testing.T) {
	err := runSubscribe([]string{"--insecure", "--ramp", "-5", "10"})
	assert.ErrorContains(t, err, "--ramp must be >= 0")
}
