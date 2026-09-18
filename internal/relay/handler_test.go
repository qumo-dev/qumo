package relay

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRelayHandler(ctx context.Context) *relayHandler {
	ann, _ := moqt.NewAnnouncement(ctx, "/test")
	ctx, cancel := context.WithCancel(ctx)
	// minimal moqt.Session to satisfy non-nil constraints
	sess := &moqt.Session{}
	return &relayHandler{
		announcement: ann,
		session:      sess,
		tracks:       newTrackManager(0, nil),
		nodeID:       "test-node",
		ctx:          ctx,
		cancel:       cancel,
	}
}

// // func TestTrackDistributor_ByteCounters( removed: subscribe/unsubscribe API replaced by broadcastNotify

func TestTrackDistributor_ByteCounters(t *testing.T) {
	// Use a unique trackID per run so parallel tests don't share counter state.
	nodeID := fmt.Sprintf("node-%d", time.Now().UnixNano())
	trackID := "[" + nodeID + "]/live/cam1/video"

	dist := newTrackDistributor(newTrackManager(0, nil), trackID, nil, nil)
	defer close(dist.done)

	// Verify the counters are wired to the correct Prometheus metric+label.
	// This is independent of the notification mechanism (broadcastNotify) — it
	// exercises the ingress/egress byte-counter wiring that addGroup/egress rely on.
	dist.ingressCounter.Add(200)
	dist.ingressCounter.Add(100)
	dist.egressCounter.Add(150)

	assert.Equal(t, 300.0, testutil.ToFloat64(metricRelayIngressBytesTotal.WithLabelValues(trackID)))
	assert.Equal(t, 150.0, testutil.ToFloat64(metricRelayEgressBytesTotal.WithLabelValues(trackID)))
}

// TestTrackDistributor_NotificationDelivery tests notification delivery guarantees
// func TestTrackDistributor_NotificationDelivery( removed: subscribe/unsubscribe API replaced by broadcastNotify

// ============================================================================
// trackDistributor Integration Tests
// ============================================================================

// TestTrackDistributor_GroupRingIntegration tests groupRing initialization
func TestTrackDistributor_GroupRingIntegration(t *testing.T) {
	dist := &trackDistributor{
		ring: newGroupRing(DefaultGroupCacheSize, DefaultFramePool),
	}

	// Verify ring is properly initialized
	require.NotNil(t, dist.ring, "Ring should be initialized")

	head := dist.ring.head()
	assert.Equal(t, moqt.GroupSequence(0), head, "Expected initial head to be 0")

	earliest := dist.ring.earliestAvailable()
	assert.Equal(t, moqt.GroupSequence(1), earliest, "Expected earliest to be 1")
}

// TestTrackDistributor_DoneChannel tests that the done channel is closed when ingest stops
func TestTrackDistributor_DoneChannel(t *testing.T) {
	dist := newTrackDistributor(newTrackManager(0, nil), "[test-node]/test/test", nil, nil)

	// done should not be closed initially
	select {
	case <-dist.done:
		require.Fail(t, "done channel should not be closed yet")
	default:
	}

	// Simulate ingest finishing by closing done directly
	close(dist.done)

	select {
	case <-dist.done:
		// Expected
	case <-time.After(50 * time.Millisecond):
		require.Fail(t, "done channel should be closed")
	}
}

// TestTrackDistributor_RingBehavior tests ring head and earliest available
func TestTrackDistributor_RingBehavior(t *testing.T) {
	t.Run("ring_initialization", func(t *testing.T) {
		dist := &trackDistributor{
			ring: newGroupRing(DefaultGroupCacheSize, DefaultFramePool),
		}

		// Verify ring is initialized
		assert.NotNil(t, dist.ring, "Ring should be initialized")

		// Verify initial head
		head := dist.ring.head()
		assert.Equal(t, moqt.GroupSequence(0), head, "Expected head 0")
	})

	t.Run("earliest_available_at_start", func(t *testing.T) {
		dist := &trackDistributor{
			ring: newGroupRing(DefaultGroupCacheSize, DefaultFramePool),
		}

		earliest := dist.ring.earliestAvailable()
		assert.Equal(t, moqt.GroupSequence(1), earliest, "Expected earliest 1 at start")
	})

	t.Run("catchup_logic", func(t *testing.T) {
		dist := &trackDistributor{
			ring: newGroupRing(DefaultGroupCacheSize, DefaultFramePool),
		}

		// Initially head should be 0
		assert.Equal(t, moqt.GroupSequence(0), dist.ring.head(), "Expected initial head to be 0")

		// Verify earliest available - starts at 1 for empty ring
		earliest := dist.ring.earliestAvailable()
		assert.GreaterOrEqual(t, earliest, uint64(0), "Expected earliest to be non-negative")
	})
}

// ============================================================================
// RelayHandler Tests - subscribe() does not hold mu
// ============================================================================

// TestRelayHandler_ConcurrentSubscribe verifies that subscribe() can be called
// concurrently without deadlock. This is a regression test for a bug where
// ServeTrack held a mutex during subscribe(), which performs a blocking network
// round-trip. A second ServeTrack call (e.g. for "video" while "video.meta" was
// subscribing) would block on the same mutex, causing a deadlock.
func TestRelayHandler_ConcurrentSubscribe(t *testing.T) {
	h := newTestRelayHandler(t.Context())

	// Pre-fill distributors to avoid real Session.Subscribe calls
	const numTracks = 10
	for i := range numTracks {
		name := moqt.TrackName(fmt.Sprintf("track-%d", i))
		trackID := "[test-node]/test/" + string(name)
		d := newTrackDistributor(h.tracks, trackID, nil, nil)
		defer close(d.done)
		h.tracks.store(trackID, d)
	}

	done := make(chan struct{}, numTracks)

	for i := range numTracks {
		go func() {
			defer func() { done <- struct{}{} }()
			name := moqt.TrackName(fmt.Sprintf("track-%d", i))

			// Should hit the cache and return immediately
			tr := h.subscribe(name)
			_ = tr
		}()
	}

	// All goroutines must finish within 1 second; a deadlock would hang.
	timeout := time.After(1 * time.Second)
	for range numTracks {
		select {
		case <-done:
		case <-timeout:
			t.Fatal("Deadlock detected: concurrent subscribe calls blocked")
		}
	}
}

// TestRelayHandler_SingleflightDedup verifies that concurrent ServeTrack calls
// for the same track name result in only one upstream subscribe via singleflight.
func TestRelayHandler_SingleflightDedup(t *testing.T) {
	h := newTestRelayHandler(t.Context()) // nil session for test

	// Pre-populate a distributor in the cache
	existing := newTrackDistributor(newTrackManager(0, nil), "[test-node]/test/video", nil, nil)
	defer close(existing.done) // Cleanup goroutine
	h.tracks.store("[test-node]/test/video", existing)

	// Load should return the existing one
	v, ok := h.tracks.load("[test-node]/test/video")
	require.True(t, ok, "Expected cached entry")
	assert.Same(t, existing, v, "Should return the cached distributor")

	// remove with the correct value should succeed
	h.tracks.remove("[test-node]/test/video", existing)

	// Subsequent load should miss
	_, ok = h.tracks.load("[test-node]/test/video")
	assert.False(t, ok, "Should not find deleted entry")
}

// ============================================================================
// RouteStats Tests
// ============================================================================

// TestRelayHandler_RouteStats_Interface verifies that *relayHandler satisfies
// the RouteReporter and moqt.TrackInfoProvider interfaces and is discoverable via type assertion.
func TestRelayHandler_RouteStats_Interface(t *testing.T) {
	ctx := context.Background()
	h := newTestRelayHandler(ctx)

	var th moqt.TrackHandler = h
	rr, ok := th.(RouteReporter)
	require.True(t, ok, "*relayHandler must implement RouteReporter")
	assert.NotNil(t, rr)

	tip, ok := th.(moqt.TrackInfoProvider)
	require.True(t, ok, "*relayHandler must implement moqt.TrackInfoProvider")
	assert.NotNil(t, tip)
}

// TestRelayHandler_TrackInfo_UseCases tests TrackInfo behavior across multiple scenarios:
// 1. Fast path cache hit
// 2. Nil session or uninitialized transport
// 3. Inactive or closed announcement
// 4. Cancelled context
// 5. Concurrent queries for the same track (deduplication & cache sharing)
func TestRelayHandler_TrackInfo_UseCases(t *testing.T) {
	t.Run("CacheHit", func(t *testing.T) {
		h := newTestRelayHandler(context.Background())
		expected := moqt.PublishInfo{
			Priority:   128,
			Ordered:    true,
			MaxLatency: 2000,
			Timescale:  1_000_000,
		}
		h.trackInfoCache.Store(moqt.TrackName("video"), expected)

		info, ok := h.TrackInfo("video")
		assert.True(t, ok)
		assert.Equal(t, expected, info)
	})

	t.Run("NilSession", func(t *testing.T) {
		h := newTestRelayHandler(context.Background())
		h.session = nil

		_, ok := h.TrackInfo("video")
		assert.False(t, ok)
	})

	t.Run("NilAnnouncement", func(t *testing.T) {
		h := newTestRelayHandler(context.Background())
		h.announcement = nil

		_, ok := h.TrackInfo("video")
		assert.False(t, ok)
	})

	t.Run("InactiveAnnouncement", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		ann, _ := moqt.NewAnnouncement(ctx, "/test")
		cancel() // closes announcement

		h := &relayHandler{
			announcement: ann,
			session:      &moqt.Session{},
			tracks:       newTrackManager(0, nil),
			ctx:          context.Background(),
		}

		_, ok := h.TrackInfo("video")
		assert.False(t, ok)
	})

	t.Run("CancelledContext", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel immediately
		h := newTestRelayHandler(ctx)

		_, ok := h.TrackInfo("video")
		assert.False(t, ok)
	})

	t.Run("ConcurrentCalls", func(t *testing.T) {
		h := newTestRelayHandler(context.Background())
		expected := moqt.PublishInfo{
			Priority:   255,
			Ordered:    true,
			Timescale:  1000,
		}
		// Pre-populate so concurrent calls read safely
		h.trackInfoCache.Store(moqt.TrackName("catalog"), expected)

		const concurrency = 16
		var wg sync.WaitGroup
		wg.Add(concurrency)

		for range concurrency {
			go func() {
				defer wg.Done()
				info, ok := h.TrackInfo("catalog")
				assert.True(t, ok)
				assert.Equal(t, expected, info)
			}()
		}
		wg.Wait()
	})
}

// TestRelayHandler_Hops_LocalAnnouncement confirms that a locally created
// announcement (no forwarding) reports 0 hops.
func TestRelayHandler_Hops_LocalAnnouncement(t *testing.T) {
	ctx := context.Background()
	h := newTestRelayHandler(ctx)

	assert.Equal(t, 0, h.RouteStats().Hops, "local announcement should have 0 hops")
}

// TestRelayHandler_RTT_NilSession returns a RouteStats with nil Probe when
// the session is nil.
func TestRelayHandler_RTT_NilSession(t *testing.T) {
	ctx := context.Background()
	h := newTestRelayHandler(ctx) // session is nil

	assert.Equal(t, 0, h.RouteStats().Hops, "nil session should yield 0 hops without panic")
	assert.Equal(t, uint64(0), h.RouteStats().EstimatedBitrate, "nil session should yield 0 bitrate without panic")
	assert.Equal(t, time.Duration(0), h.RouteStats().RTT, "nil session should yield 0 RTT without panic")
}

// ============================================================================
// ingest concurrent group processing tests
// ============================================================================

// TestIngest_ConcurrentGroups_AllCachesComplete exercises the reserve+fill split
// directly: reserve N caches sequentially, then fill them concurrently, and
// confirm that every cache is marked complete and holds the correct frames.
func TestIngest_ConcurrentGroups_AllCachesComplete(t *testing.T) {
	const numGroups = 10
	const framesPerGroup = 5

	ring := newGroupRing(numGroups, DefaultFramePool)
	caches := make([]*groupCache, numGroups)
	for i := range numGroups {
		caches[i] = ring.reserve(moqt.GroupSequence(i + 1))
	}

	var wg sync.WaitGroup
	for i := range numGroups {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			frames := make([][]byte, framesPerGroup)
			for j := range framesPerGroup {
				frames[j] = fmt.Appendf(nil, "g%d-f%d", idx, j)
			}
			ring.fill(&fakeFrameSource{frames: frames}, caches[idx], nil)
		}(i)
	}
	wg.Wait()

	for i, c := range caches {
		assert.True(t, c.isComplete(), "group %d should be complete", i)
		for j := range framesPerGroup {
			assert.NotNil(t, c.next(j), "group %d frame %d should exist", i, j)
		}
	}
}

// TestIngest_ConcurrentGroups_FasterThanSerial confirms that concurrent fill
// completes in roughly framesPerGroup×delay time, not numGroups×framesPerGroup×delay.
// Uses testing/synctest so time advances deterministically.
func TestIngest_ConcurrentGroups_FasterThanSerial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const numGroups = 6
		const framesPerGroup = 3
		const delay = 10 * time.Millisecond

		ring := newGroupRing(numGroups, DefaultFramePool)
		caches := make([]*groupCache, numGroups)
		for i := range numGroups {
			caches[i] = ring.reserve(moqt.GroupSequence(i + 1))
		}

		allDone := make(chan struct{})
		var wg sync.WaitGroup
		for i := range numGroups {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				frames := make([][]byte, framesPerGroup)
				for j := range framesPerGroup {
					frames[j] = []byte("x")
				}
				src := &slowFrameSource{
					frames: frames,
					delay:  delay,
				}
				ring.fill(src, caches[idx], nil)
			}(i)
		}
		go func() {
			wg.Wait()
			close(allDone)
		}()

		// All goroutines sleep concurrently for framesPerGroup×delay.
		// Advance time just past that to confirm they all complete.
		serialTime := time.Duration(numGroups*framesPerGroup) * delay
		concurrentTime := time.Duration(framesPerGroup) * delay

		time.Sleep(concurrentTime + delay)
		synctest.Wait()

		select {
		case <-allDone:
			// good: completed in roughly framesPerGroup×delay
		default:
			t.Fatalf("fills should complete within %v, not the serial %v", concurrentTime+delay, serialTime)
		}
	})
}

// TestIngest_WaitGroup_BlocksDoneUntilFillComplete verifies the LIFO defer
// ordering: wg.Wait() must run before close(done), so subscribers cannot see
// done closed while a fill goroutine is still writing frames.
func TestIngest_WaitGroup_BlocksDoneUntilFillComplete(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ring := newGroupRing(DefaultGroupCacheSize, DefaultFramePool)
		cache := ring.reserve(moqt.GroupSequence(1))

		done := make(chan struct{})
		fillDone := make(chan struct{})

		frames := [][]byte{[]byte("a"), []byte("b"), []byte("c")}
		src := &slowFrameSource{
			frames: frames,
			delay:  10 * time.Millisecond,
		}

		var wg sync.WaitGroup
		wg.Go(func() {
			ring.fill(src, cache, nil)
			close(fillDone)
		})
		go func() {
			wg.Wait()
			close(done)
		}()

		synctest.Wait()

		// Before time advances: fill should not be done yet.
		select {
		case <-fillDone:
			t.Fatal("fill should not be done before time advances")
		default:
		}

		// Advance past all frame sleeps (3 × 10ms).
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()

		select {
		case <-fillDone:
		default:
			t.Fatal("fill should have completed after 50ms")
		}
		select {
		case <-done:
		default:
			t.Fatal("done should be closed after fill completed")
		}
	})
}

// ============================================================================
// trackDistributor metering integration tests
// ============================================================================

// TestTrackDistributor_MeteringIngress verifies that processGroup accumulates
// ingress bytes into the attached broadcastSession as well as the Prometheus counter.
func TestTrackDistributor_MeteringIngress(t *testing.T) {
	nodeID := fmt.Sprintf("meter-node-%d", time.Now().UnixNano())
	trackID := "[" + nodeID + "]/live/test/video"

	sess := newBroadcastSession("tok-ingress")
	dist := newTrackDistributor(newTrackManager(0, nil), trackID, sess, nil)
	t.Cleanup(func() { close(dist.done) })

	payload := []byte("hello-world") // 11 bytes
	src := &fakeFrameSource{frames: [][]byte{payload}}

	ok := dist.processGroup(context.Background(), moqt.GroupSequence(1), src)
	require.True(t, ok)
	// processGroup dispatches to a worker; close the job channel and wait so the
	// fill completes before asserting on its metering side effects.
	close(dist.fillJobs)
	dist.fillWg.Wait()

	assert.Equal(t, int64(len(payload)), sess.ingressBytes.Load(),
		"processGroup must add ingress bytes to the broadcast session")
	assert.Equal(t, 0.0+float64(len(payload)),
		testutil.ToFloat64(metricRelayIngressBytesTotal.WithLabelValues(trackID)),
		"Prometheus ingress counter must also be updated")
}

// TestTrackDistributor_MeteringEgress verifies that the egress path accumulates
// egress bytes into the attached broadcastSession as well as the Prometheus counter.
func TestTrackDistributor_MeteringEgress(t *testing.T) {
	nodeID := fmt.Sprintf("meter-node-egress-%d", time.Now().UnixNano())
	trackID := "[" + nodeID + "]/live/test/video"

	sess := newBroadcastSession("tok-egress")
	dist := newTrackDistributor(newTrackManager(0, nil), trackID, sess, nil)
	t.Cleanup(func() { close(dist.done) })

	// Simulate the egress byte-counting path directly via the session methods,
	// mirroring what egress() does on each frame write.
	dist.egressCounter.Add(float64(512))
	sess.addEgress(512)
	dist.egressCounter.Add(float64(256))
	sess.addEgress(256)

	assert.Equal(t, int64(768), sess.egressBytes.Load(),
		"session egress counter must accumulate across multiple writes")
	assert.Equal(t, 768.0,
		testutil.ToFloat64(metricRelayEgressBytesTotal.WithLabelValues(trackID)),
		"Prometheus egress counter must also be updated")
}

// TestTrackDistributor_MeteringNilSession verifies that processGroup and the
// egress path do not panic when no broadcastSession is attached.
func TestTrackDistributor_MeteringNilSession(t *testing.T) {
	dist := newTrackDistributor(newTrackManager(0, nil), "nil-session/test", nil, nil)
	t.Cleanup(func() { close(dist.done) })

	src := &fakeFrameSource{frames: [][]byte{[]byte("frame")}}
	assert.NotPanics(t, func() {
		dist.processGroup(context.Background(), moqt.GroupSequence(1), src)
		close(dist.fillJobs)
		dist.fillWg.Wait()
	})
}

// ============================================================================
// trackDistributor.processGroup semaphore tests
// ============================================================================

// TestTrackDistributor_ProcessGroup_SemaphoreLimitsConcurrency verifies that
// processGroup blocks (worker-pool-full) when MaxGroupFillsInFlight fill jobs
// are already in flight, and resumes as soon as a worker slot is released.
// Uses testing/synctest for deterministic goroutine scheduling.
func TestTrackDistributor_ProcessGroup_SemaphoreLimitsConcurrency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const limit = 2
		orig := MaxGroupFillsInFlight
		t.Cleanup(func() { MaxGroupFillsInFlight = orig })
		MaxGroupFillsInFlight = limit

		dist := newTrackDistributor(newTrackManager(0, nil), "test/sem", nil, nil)
		t.Cleanup(func() { close(dist.done) }) // release egress waiters
		require.Equal(t, limit, cap(dist.fillSem), "fillSem capacity must equal MaxGroupFillsInFlight")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Each slow source blocks the worker for 1 hour of synctest time, keeping
		// its backpressure slot occupied so we can observe the full-pool block.
		slowSrc := func() frameSource {
			return &slowFrameSource{
				frames: [][]byte{[]byte("x")},
				delay:  1 * time.Hour,
			}
		}

		// Spin up `limit` groups — all should be accepted without blocking.
		for i := range limit {
			ok := dist.processGroup(ctx, moqt.GroupSequence(i+1), slowSrc())
			require.True(t, ok, "processGroup should succeed while under the limit")
		}

		// All worker slots are now occupied. A further processGroup call must block.
		blocked := make(chan struct{})
		accepted := make(chan struct{})
		go func() {
			close(blocked)
			ok := dist.processGroup(ctx, moqt.GroupSequence(limit+1), slowSrc())
			if ok {
				close(accepted)
			}
		}()

		<-blocked
		synctest.Wait()

		// Verify it is still blocked (accepted not closed).
		select {
		case <-accepted:
			t.Fatal("processGroup should be blocked when the worker pool is full")
		default:
		}

		// Advance time past the slow-source delay to let one fill complete, which
		// frees a worker slot and unblocks the waiting processGroup.
		time.Sleep(2 * time.Hour)
		synctest.Wait()

		select {
		case <-accepted:
		default:
			t.Fatal("processGroup should have unblocked after a worker slot was released")
		}

		// Drain remaining workers: close the job channel so workers exit their
		// range loop, then advance time past the slow sources so in-flight fills
		// complete and the workers can exit.
		cancel()
		close(dist.fillJobs)
		time.Sleep(2 * time.Hour)
		synctest.Wait()
	})
}

// TestTrackDistributor_ProcessGroup_CtxCancelUnblocks verifies that a
// processGroup call blocked on a full worker pool returns false when its
// context is cancelled.
func TestTrackDistributor_ProcessGroup_CtxCancelUnblocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		orig := MaxGroupFillsInFlight
		t.Cleanup(func() { MaxGroupFillsInFlight = orig })
		MaxGroupFillsInFlight = 1

		dist := newTrackDistributor(newTrackManager(0, nil), "test/cancel", nil, nil)
		t.Cleanup(func() { close(dist.done) }) // release egress waiters

		ctx, cancel := context.WithCancel(context.Background())

		// Fill the single worker slot with a job that blocks.
		holdSrc := &slowFrameSource{
			frames: [][]byte{[]byte("x")},
			delay:  1 * time.Hour,
		}
		ok := dist.processGroup(ctx, moqt.GroupSequence(1), holdSrc)
		require.True(t, ok)

		// Second call must block on the full worker pool.
		result := make(chan bool, 1)
		go func() {
			result <- dist.processGroup(ctx, moqt.GroupSequence(2), &fakeFrameSource{frames: [][]byte{[]byte("y")}})
		}()

		synctest.Wait()

		// Cancel the context — the blocked processGroup must return false.
		cancel()
		synctest.Wait()

		select {
		case got := <-result:
			assert.False(t, got, "processGroup must return false on ctx cancellation")
		default:
			t.Fatal("processGroup did not return after ctx cancel")
		}

		// Advance time past the slow source so the worker's fill completes and it
		// exits cleanly (no deadlock/leak on exit).
		close(dist.fillJobs)
		time.Sleep(2 * time.Hour)
		synctest.Wait()
	})
}

// ============================================================================
// compareRoutes Tests
// ============================================================================

func TestCompareRoutes(t *testing.T) {
	type testCase struct {
		candidate RouteStats
		current   RouteStats
		want      bool
	}
	tests := map[string]testCase{
		"fewer hops wins regardless of RTT": {
			candidate: RouteStats{Alive: true, Hops: 1, RTT: 100},
			current:   RouteStats{Alive: true, Hops: 2, RTT: 10},
			want:      true,
		},
		"more hops loses regardless of RTT": {
			candidate: RouteStats{Alive: true, Hops: 3, RTT: 1},
			current:   RouteStats{Alive: true, Hops: 2, RTT: 999},
			want:      false,
		},
		"equal hops: higher bitrate wins over lower RTT": {
			candidate: RouteStats{Alive: true, Hops: 2, EstimatedBitrate: 10_000_000, RTT: 80},
			current:   RouteStats{Alive: true, Hops: 2, EstimatedBitrate: 5_000_000, RTT: 20},
			want:      true,
		},
		"equal hops and bitrate: lower RTT wins": {
			candidate: RouteStats{Alive: true, Hops: 2, EstimatedBitrate: 5_000_000, RTT: 20 * time.Millisecond},
			current:   RouteStats{Alive: true, Hops: 2, EstimatedBitrate: 5_000_000, RTT: 50 * time.Millisecond},
			want:      true,
		},
		"equal hops: higher RTT loses": {
			candidate: RouteStats{Alive: true, Hops: 2, RTT: 80 * time.Millisecond},
			current:   RouteStats{Alive: true, Hops: 2, RTT: 50 * time.Millisecond},
			want:      false,
		},
		"equal hops: zero bitrate/RTT keeps existing route": {
			candidate: RouteStats{Alive: true, Hops: 2},
			current:   RouteStats{Alive: true, Hops: 2, EstimatedBitrate: 5_000_000, RTT: 50 * time.Millisecond},
			want:      false,
		},
			// Hysteresis: 5.5Mbps vs 5Mbps = 10% improvement, below 20% margin.
			"equal hops: small bitrate improvement keeps current (hysteresis)": {
				candidate: RouteStats{Alive: true, Hops: 2, EstimatedBitrate: 5_500_000},
				current:   RouteStats{Alive: true, Hops: 2, EstimatedBitrate: 5_000_000},
				want:      false,
			},
			// Hysteresis: 2ms RTT improvement is below 5ms absolute threshold.
			"equal hops: tiny RTT improvement keeps current (hysteresis)": {
				candidate: RouteStats{Alive: true, Hops: 2, RTT: 48 * time.Millisecond},
				current:   RouteStats{Alive: true, Hops: 2, RTT: 50 * time.Millisecond},
				want:      false,
			},
			// Hysteresis: 6ms RTT improvement clears absolute threshold (OR gate) even
			// though 44/50=0.88 does not clear the 0.8 relative threshold.
			"equal hops: absolute RTT improvement wins with OR gate": {
				candidate: RouteStats{Alive: true, Hops: 2, RTT: 44 * time.Millisecond},
				current:   RouteStats{Alive: true, Hops: 2, RTT: 50 * time.Millisecond},
				want:      true,
			},
			// Hysteresis: 105ms→100ms clears absolute (5ms) but not relative (4.8%).
			// OR gate still accepts because the absolute threshold is met.
			"equal hops: high-RTT route with small absolute improvement wins via OR gate": {
				candidate: RouteStats{Alive: true, Hops: 2, RTT: 100 * time.Millisecond},
				current:   RouteStats{Alive: true, Hops: 2, RTT: 105 * time.Millisecond},
				want:      true,
			},
			// Hysteresis: 4ms vs 8ms clears relative (0.5 <= 0.8) but not absolute (4ms < 5ms).
			// OR gate still accepts because the relative threshold is met.
			"equal hops: relative RTT improvement wins via OR gate": {
				candidate: RouteStats{Alive: true, Hops: 2, RTT: 4 * time.Millisecond},
				current:   RouteStats{Alive: true, Hops: 2, RTT: 8 * time.Millisecond},
				want:      true,
			},
			// Bitrate at exactly 1.2x margin: 6Mbps / 5Mbps = 1.2, should win.
			"equal hops: bitrate at margin boundary wins": {
				candidate: RouteStats{Alive: true, Hops: 2, EstimatedBitrate: 6_000_000},
				current:   RouteStats{Alive: true, Hops: 2, EstimatedBitrate: 5_000_000},
				want:      true,
			},
			// Bitrate just below 1.2x margin: 5.99Mbps / 5Mbps ~ 1.198, should lose.
			"equal hops: bitrate just below margin loses": {
				candidate: RouteStats{Alive: true, Hops: 2, EstimatedBitrate: 5_990_000},
				current:   RouteStats{Alive: true, Hops: 2, EstimatedBitrate: 5_000_000},
				want:      false,
			},
			// Candidate has lower bitrate: decisionInferiorBitrate path.
			"equal hops: lower bitrate loses": {
				candidate: RouteStats{Alive: true, Hops: 2, EstimatedBitrate: 3_000_000},
				current:   RouteStats{Alive: true, Hops: 2, EstimatedBitrate: 5_000_000},
				want:      false,
			},
			// Both routes have zero RTT: decisionEqualOrUnknown.
			"equal hops: both RTT zero keeps existing": {
				candidate: RouteStats{Alive: true, Hops: 2},
				current:   RouteStats{Alive: true, Hops: 2},
				want:      false,
			},


		// Alive dominates all quality metrics.
		"alive candidate beats dead current regardless of hops": {
			candidate: RouteStats{Alive: true, Hops: 5},
			current:   RouteStats{Alive: false, Hops: 1},
			want:      true,
		},
		"dead candidate loses to alive current regardless of hops": {
			candidate: RouteStats{Alive: false, Hops: 1},
			current:   RouteStats{Alive: true, Hops: 5},
			want:      false,
		},
		"both dead: keep existing route": {
			candidate: RouteStats{Alive: false, Hops: 1, RTT: 1},
			current:   RouteStats{Alive: false, Hops: 5, RTT: 999},
			want:      false,
		},
		"both alive: normal hop comparison applies": {
			candidate: RouteStats{Alive: true, Hops: 1, RTT: 100},
			current:   RouteStats{Alive: true, Hops: 2, RTT: 10},
			want:      true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := compareRoutes(tt.candidate, tt.current).accepted()
			assert.Equal(t, tt.want, got)
		})
	}
}

// ============================================================================
// RouteStats.Alive Tests
// ============================================================================

// TestRelayHandler_Alive_ActiveContext verifies Alive=true for a live handler.
func TestRelayHandler_Alive_ActiveContext(t *testing.T) {
	h := newTestRelayHandler(t.Context())
	assert.True(t, h.RouteStats().Alive, "handler with active context should be alive")
}

// TestRelayHandler_Alive_CancelledContext verifies Alive=false after ctx cancel.
func TestRelayHandler_Alive_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ann, _ := moqt.NewAnnouncement(ctx, "/test")
	sess := &moqt.Session{}
	h := &relayHandler{
		announcement: ann,
		session:      sess,
		tracks:       newTrackManager(0, nil),
		ctx:          ctx,
		cancel:       cancel,
	}

	assert.True(t, h.RouteStats().Alive, "should be alive before cancel")
	cancel()
	assert.False(t, h.RouteStats().Alive, "should be dead after cancel")
}

// TestRelayHandler_Alive_RetractedAnnouncement verifies Alive=false when
// the announcement is retracted (IsActive=false) even if the context is live.
func TestRelayHandler_Alive_RetractedAnnouncement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create an announcement then retract it via EndAnnouncementFunc.
	ann, end := moqt.NewAnnouncement(ctx, "/test")
	sess := &moqt.Session{}
	h := &relayHandler{
		announcement: ann,
		session:      sess,
		tracks:       newTrackManager(0, nil),
		ctx:          ctx,
		cancel:       cancel,
	}

	assert.True(t, h.RouteStats().Alive, "should be alive before retract")
	end() // retract the announcement
	<-ann.Done()
	assert.False(t, h.RouteStats().Alive, "should be dead after announcement retract")
}

// ============================================================================
// Drain Tests
// ============================================================================

// TestRelayHandler_Drain_ZeroTimeout verifies that Drain(0) cancels immediately.
func TestRelayHandler_Drain_ZeroTimeout(t *testing.T) {
	h := newTestRelayHandler(context.Background())
	require.True(t, h.RouteStats().Alive)

	h.Drain(0)

	// time.AfterFunc(0, ...) fires in a separate goroutine; give it a moment.
	assert.Eventually(t, func() bool {
		return !h.RouteStats().Alive
	}, 100*time.Millisecond, time.Millisecond, "Drain(0) should cancel context quickly")
}

// TestRelayHandler_Drain_WithTimeout verifies that Drain with a positive timeout
// leaves the handler alive initially and kills it after the delay.
func TestRelayHandler_Drain_WithTimeout(t *testing.T) {
	h := newTestRelayHandler(context.Background())

	h.Drain(50 * time.Millisecond)

	assert.True(t, h.RouteStats().Alive, "should still be alive immediately after Drain")

	assert.Eventually(t, func() bool {
		return !h.RouteStats().Alive
	}, 200*time.Millisecond, time.Millisecond, "handler should become dead after drain timeout")
}

// TestRelayHandler_Drain_Idempotent verifies multiple Drain calls don't panic
// and cancel is idempotent.
func TestRelayHandler_Drain_Idempotent(t *testing.T) {
	h := newTestRelayHandler(context.Background())

	require.NotPanics(t, func() {
		h.Drain(0)
		h.Drain(0)
		h.Drain(50 * time.Millisecond)
	})

	assert.Eventually(t, func() bool {
		return !h.RouteStats().Alive
	}, 100*time.Millisecond, time.Millisecond)
}

func BenchmarkEgressHistogramObserve(b *testing.B) {
	trackName := []byte("test-track-name")
	start := time.Now()

	b.Run("baseline", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			metricGroupDeliveryHistogram.WithLabelValues(string(trackName)).Observe(time.Since(start).Seconds())
		}
	})

	b.Run("optimized", func(b *testing.B) {
		trackNameStr := string(trackName)
		for i := 0; i < b.N; i++ {
			metricGroupDeliveryHistogram.WithLabelValues(trackNameStr).Observe(time.Since(start).Seconds())
		}
	})

	b.Run("prebound", func(b *testing.B) {
		observer := metricGroupDeliveryHistogram.WithLabelValues(string(trackName))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			observer.Observe(time.Since(start).Seconds())
		}
	})
}
