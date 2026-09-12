package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestMeter returns a Meter backed by srv with a configurable tick interval.
func newTestMeter(srv *httptest.Server, interval time.Duration) *Meter {
	client := &CredentialClient{
		baseURL:    srv.URL,
		httpClient: srv.Client(),
		cache:      make(map[string]cachedCredential),
	}
	m := newMeter(client)
	m.interval = interval
	return m
}

// collectUsageEvents captures all UsageEvent batches POSTed to /v1/usage/events.
// It returns the handler and a pointer to the collected slice (guarded by mu).
func collectUsageEvents(mu *sync.Mutex, out *[]UsageEvent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage/events" {
			return
		}
		var batch []UsageEvent
		_ = json.NewDecoder(r.Body).Decode(&batch)
		mu.Lock()
		*out = append(*out, batch...)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}
}

// ── UUID ──────────────────────────────────────────────────────────────────────

var uuidV4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestBroadcastSession_ID_Format(t *testing.T) {
	for range 20 {
		s := newBroadcastSession("test-token")
		assert.Regexp(t, uuidV4Re, s.id.String(), "UUID must match RFC 4122 v4 format")
	}
}

func TestBroadcastSession_ID_Uniqueness(t *testing.T) {
	seen := make(map[uuid.UUID]struct{}, 200)
	for range 200 {
		s := newBroadcastSession("test-token")
		_, dup := seen[s.id]
		assert.False(t, dup, "newBroadcastSession must not produce duplicate IDs")
		seen[s.id] = struct{}{}
	}
}

// ── broadcastSession ─────────────────────────────────────────────────────────

func TestNewBroadcastSession(t *testing.T) {
	s := newBroadcastSession("owner-tok")
	require.NotNil(t, s)
	assert.Equal(t, "owner-tok", s.ownerTokenID)
	assert.Regexp(t, uuidV4Re, s.id.String(), "session ID must be a valid UUID v4")
	assert.Zero(t, s.ingressBytes.Load(), "ingress counter must start at zero")
	assert.Zero(t, s.egressBytes.Load(), "egress counter must start at zero")
}

func TestBroadcastSession_Counters(t *testing.T) {
	s := newBroadcastSession("tok")
	s.addIngress(500)
	s.addIngress(300)
	s.addEgress(1000)
	s.addEgress(24)

	assert.Equal(t, int64(800), s.ingressBytes.Load())
	assert.Equal(t, int64(1024), s.egressBytes.Load())
}

func TestBroadcastSession_CountersConcurrent(t *testing.T) {
	s := newBroadcastSession("tok")
	const goroutines = 50

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)
	for range goroutines {
		go func() { defer wg.Done(); s.addIngress(1) }()
		go func() { defer wg.Done(); s.addEgress(2) }()
	}
	wg.Wait()

	assert.Equal(t, int64(goroutines), s.ingressBytes.Load())
	assert.Equal(t, int64(goroutines*2), s.egressBytes.Load())
}

func TestBroadcastSession_ToEvent(t *testing.T) {
	s := newBroadcastSession("owner-x")
	s.addIngress(128)
	s.addEgress(512)

	before := time.Now()
	event := s.toEvent()
	after := time.Now()

	assert.Equal(t, s.id.String(), event.BroadcastSessionID)
	assert.Equal(t, "owner-x", event.OwnerTokenID)
	assert.Equal(t, int64(128), event.Metrics["gateway.ingress_bytes"])
	assert.Equal(t, int64(512), event.Metrics["gateway.egress_bytes"])

	ts, err := time.Parse(time.RFC3339, event.Ts)
	require.NoError(t, err, "Ts must be a valid RFC3339 timestamp")
	assert.False(t, ts.Before(before.Truncate(time.Second)))
	assert.False(t, ts.After(after.Add(time.Second)))
}

func TestBroadcastSession_ToEvent_ReflectsLatestCounters(t *testing.T) {
	s := newBroadcastSession("tok")
	s.addIngress(100)
	e1 := s.toEvent()
	s.addIngress(200)
	e2 := s.toEvent()

	assert.Equal(t, int64(100), e1.Metrics["gateway.ingress_bytes"])
	assert.Equal(t, int64(300), e2.Metrics["gateway.ingress_bytes"])
}

// ── Meter.Register / Deregister ──────────────────────────────────────────────

func TestMeter_Register_AddsToActiveSet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := newTestMeter(srv, time.Hour)
	s1 := newBroadcastSession("tok-a")
	s2 := newBroadcastSession("tok-b")

	m.Register(s1)
	m.Register(s2)

	m.mu.Lock()
	_, has1 := m.sessions[s1]
	_, has2 := m.sessions[s2]
	count := len(m.sessions)
	m.mu.Unlock()

	assert.True(t, has1)
	assert.True(t, has2)
	assert.Equal(t, 2, count)
}

func TestMeter_Deregister_RemovesFromActiveSetAndSendsFinalReport(t *testing.T) {
	var mu sync.Mutex
	var received []UsageEvent
	srv := httptest.NewServer(collectUsageEvents(&mu, &received))
	defer srv.Close()

	m := newTestMeter(srv, time.Hour)
	sess := newBroadcastSession("tok-final")
	sess.addIngress(1000)
	sess.addEgress(4000)

	m.Register(sess)
	m.Deregister(context.Background(), sess)

	// Session must be removed.
	m.mu.Lock()
	_, still := m.sessions[sess]
	m.mu.Unlock()
	assert.False(t, still, "session must be removed from active set after Deregister")

	// Final report must have been sent.
	mu.Lock()
	events := received
	mu.Unlock()
	require.Len(t, events, 1, "Deregister must POST exactly one final usage event")
	assert.Equal(t, sess.id.String(), events[0].BroadcastSessionID)
	assert.Equal(t, "tok-final", events[0].OwnerTokenID)
	assert.Equal(t, int64(1000), events[0].Metrics["gateway.ingress_bytes"])
	assert.Equal(t, int64(4000), events[0].Metrics["gateway.egress_bytes"])
}

// ── Meter.report ─────────────────────────────────────────────────────────────

func TestMeter_Report_AggregatesAllActiveSessions(t *testing.T) {
	var mu sync.Mutex
	var received []UsageEvent
	srv := httptest.NewServer(collectUsageEvents(&mu, &received))
	defer srv.Close()

	m := newTestMeter(srv, time.Hour)

	s1 := newBroadcastSession("tok-1")
	s1.addIngress(100)
	s2 := newBroadcastSession("tok-2")
	s2.addIngress(200)
	s2.addEgress(800)

	m.Register(s1)
	m.Register(s2)
	m.report(context.Background())

	mu.Lock()
	events := received
	mu.Unlock()

	require.Len(t, events, 2)
	byID := make(map[string]UsageEvent, 2)
	for _, e := range events {
		byID[e.BroadcastSessionID] = e
	}
	require.Contains(t, byID, s1.id.String())
	require.Contains(t, byID, s2.id.String())
	assert.Equal(t, int64(100), byID[s1.id.String()].Metrics["gateway.ingress_bytes"])
	assert.Equal(t, int64(200), byID[s2.id.String()].Metrics["gateway.ingress_bytes"])
	assert.Equal(t, int64(800), byID[s2.id.String()].Metrics["gateway.egress_bytes"])
}

func TestMeter_Report_NoOpWhenNoSessions(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	m := newTestMeter(srv, time.Hour)
	m.report(context.Background())
	assert.False(t, called, "report must not POST when there are no active sessions")
}

func TestMeter_Report_ReflectsCurrentCounters(t *testing.T) {
	var mu sync.Mutex
	var received []UsageEvent
	srv := httptest.NewServer(collectUsageEvents(&mu, &received))
	defer srv.Close()

	m := newTestMeter(srv, time.Hour)
	sess := newBroadcastSession("tok")
	sess.addIngress(50)
	m.Register(sess)

	m.report(context.Background())

	sess.addIngress(50) // add more after first report

	m.report(context.Background())

	mu.Lock()
	events := received
	mu.Unlock()

	require.Len(t, events, 2)
	assert.Equal(t, int64(50), events[0].Metrics["gateway.ingress_bytes"], "first report must show 50")
	assert.Equal(t, int64(100), events[1].Metrics["gateway.ingress_bytes"], "second report must show cumulative 100")
}

// ── Meter.Run ────────────────────────────────────────────────────────────────

func TestMeter_Run_StopsOnContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := newTestMeter(srv, time.Hour) // long interval: no ticks during test
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run must return promptly after context cancellation")
	}
}

func TestMeter_Run_FiresPeriodicReports(t *testing.T) {
	var reportCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/usage/events" {
			reportCount.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	const tickInterval = 15 * time.Millisecond
	m := newTestMeter(srv, tickInterval)
	sess := newBroadcastSession("tok-periodic")
	m.Register(sess)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	m.Run(ctx) // blocks until ctx times out

	assert.GreaterOrEqual(t, reportCount.Load(), int32(2),
		"Run must fire at least 2 periodic reports within the test window")
}
