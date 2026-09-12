package relay

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
	"uuid"
)

// broadcastSession tracks cumulative ingress and egress bytes for a single
// announced broadcast path. It is minted at ANNOUNCE time and lives until
// the publisher session ends.
type broadcastSession struct {
	id           string // UUID v4, minted at ANNOUNCE
	ownerTokenID string // token_id from the credential introspection response

	ingressBytes atomic.Int64
	egressBytes  atomic.Int64
}

func newBroadcastSession(ownerTokenID string) *broadcastSession {
	return &broadcastSession{
		id:           newUUIDv4(),
		ownerTokenID: ownerTokenID,
	}
}

func (s *broadcastSession) addIngress(n int64) { s.ingressBytes.Add(n) }
func (s *broadcastSession) addEgress(n int64)  { s.egressBytes.Add(n) }

func (s *broadcastSession) toEvent() UsageEvent {
	return UsageEvent{
		BroadcastSessionID: s.id,
		OwnerTokenID:       s.ownerTokenID,
		Metrics: map[string]int64{
			"gateway.ingress_bytes": s.ingressBytes.Load(),
			"gateway.egress_bytes":  s.egressBytes.Load(),
		},
		Ts: time.Now().UTC().Format(time.RFC3339),
	}
}

// Meter manages active broadcast sessions and periodically reports cumulative
// usage to the backend. A single Meter is shared across all publisher sessions
// on a relay node.
type Meter struct {
	client   *CredentialClient
	interval time.Duration

	mu       sync.Mutex
	sessions map[*broadcastSession]struct{}
}

func newMeter(client *CredentialClient) *Meter {
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

func (m *Meter) report(ctx context.Context) {
	m.mu.Lock()
	events := make([]UsageEvent, 0, len(m.sessions))
	for sess := range m.sessions {
		events = append(events, sess.toEvent())
	}
	m.mu.Unlock()

	if len(events) == 0 {
		return
	}
	if err := m.client.ReportUsage(ctx, events); err != nil {
		slog.Warn("meter: periodic usage report failed", "error", err)
	}
}

// newUUIDv4 generates a random UUID v4 string using standard library uuid.
func newUUIDv4() string {
	return uuid.NewV4().String()
}
