package relay

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
	"uuid"
)

// Session roles and event types reported to the control plane
// (POST /v1/usage/events; qumo-deploy ADR 0035).
const (
	rolePublisher  = "publisher"
	roleSubscriber = "subscriber"

	eventUsage        = "usage"
	eventSessionOpen  = "session_open"
	eventSessionClose = "session_close"

	// reasonClosed is the close reason of a session nothing forced to end.
	reasonClosed = "closed"

	// maxPendingEvents bounds session events queued between reports; the
	// oldest are dropped past it.
	maxPendingEvents = 10_000
)

// broadcastSession tracks cumulative ingress and egress bytes for a single
// announced broadcast path. It is minted at ANNOUNCE time and lives until
// the publisher session ends.
type broadcastSession struct {
	id        uuid.UUID // UUID v4, minted at ANNOUNCE
	projectID string    // the admitting key's project; empty for a static key

	mu    sync.Mutex
	keyID string // kid of the publisher's current credential

	ingressBytes atomic.Int64
	egressBytes  atomic.Int64
}

func newBroadcastSession(keyID string) *broadcastSession {
	return &broadcastSession{
		id:    uuid.NewV4(),
		keyID: keyID,
	}
}

// setKey records the kid of a refreshed credential.
func (s *broadcastSession) setKey(keyID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keyID = keyID
}

func (s *broadcastSession) addIngress(n int64) { s.ingressBytes.Add(n) }
func (s *broadcastSession) addEgress(n int64)  { s.egressBytes.Add(n) }

func (s *broadcastSession) toEvent() UsageEvent {
	s.mu.Lock()
	keyID := s.keyID
	s.mu.Unlock()
	return UsageEvent{
		Type:      eventUsage,
		SessionID: s.id.String(),
		Role:      rolePublisher,
		KeyID:     keyID,
		ProjectID: s.projectID,
		Metrics: map[string]int64{
			"gateway.ingress_bytes": s.ingressBytes.Load(),
			"gateway.egress_bytes":  s.egressBytes.Load(),
		},
		Ts: time.Now().UTC().Format(time.RFC3339),
	}
}

// Meter manages active broadcast sessions and periodically reports cumulative
// usage, and session open and close events, to the backend. A single Meter is
// shared across all sessions on a relay node.
type Meter struct {
	client   *usageClient
	interval time.Duration

	mu       sync.Mutex
	sessions map[*broadcastSession]struct{}
	pending  []UsageEvent // session events not yet reported
}

// enqueue queues a session event for the next report, dropping the oldest
// past maxPendingEvents.
func (m *Meter) enqueue(events ...UsageEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = append(m.pending, events...)
	if over := len(m.pending) - maxPendingEvents; over > 0 {
		slog.Warn("meter: dropping the oldest queued session events", "dropped", over)
		m.pending = m.pending[over:]
	}
}

func newMeter(client *usageClient) *Meter {
	return &Meter{
		client:   client,
		interval: 30 * time.Second,
		sessions: make(map[*broadcastSession]struct{}),
	}
}

// Register adds a broadcast session to the active set so it is included
// in periodic usage reports.
func (m *Meter) Register(sess *broadcastSession) {
	m.mu.Lock()
	m.sessions[sess] = struct{}{}
	m.mu.Unlock()
}

// Deregister removes a session from the active set and sends its final
// cumulative usage report before returning.
func (m *Meter) Deregister(ctx context.Context, sess *broadcastSession) {
	m.mu.Lock()
	delete(m.sessions, sess)
	m.mu.Unlock()

	event := sess.toEvent()
	if err := m.client.ReportUsage(ctx, []UsageEvent{event}); err != nil {
		slog.Warn("meter: final usage report failed",
			"session_id", sess.id,
			"error", err)
	}
}

// Run starts the periodic reporter; it blocks until ctx is cancelled.
// Start it in a goroutine alongside the relay server.
func (m *Meter) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.report(ctx)
		}
	}
}

// report sends queued session events and a usage snapshot of every active
// broadcast. Session events that fail to send are queued again; usage is
// cumulative, so the next snapshot supersedes a lost one.
func (m *Meter) report(ctx context.Context) {
	m.mu.Lock()
	pending := m.pending
	m.pending = nil
	events := make([]UsageEvent, 0, len(pending)+len(m.sessions))
	events = append(events, pending...)
	for sess := range m.sessions {
		events = append(events, sess.toEvent())
	}
	m.mu.Unlock()

	if len(events) == 0 {
		return
	}
	if err := m.client.ReportUsage(ctx, events); err != nil {
		slog.Warn("meter: periodic usage report failed", "error", err)
		m.mu.Lock()
		m.pending = append(pending, m.pending...)
		m.mu.Unlock()
	}
}
