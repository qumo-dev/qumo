package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"
)

const (
	// usageFlushInterval is how often pending usage records are sent.
	usageFlushInterval = 10 * time.Second
	// usageSendTimeout bounds one send, and the last send at shutdown.
	usageSendTimeout = 10 * time.Second
	// maxBatchRecords bounds one POST, so a backlog after an outage goes out
	// in pieces a receiver's body limit accepts.
	maxBatchRecords = 500
	// maxPendingEvents caps the opens and closes waiting while the usage
	// receiver can't be reached; the oldest are dropped past it. Usage needs
	// no cap: it is one cumulative record per live session.
	maxPendingEvents = 50_000
	// sessionGrace is how long a session is remembered past its expiry, for
	// an end that arrives late.
	sessionGrace = 5 * time.Minute
)

// Usage record types and roles, as a usage receiver takes them.
const (
	recordSessionOpen  = "session_open"
	recordUsage        = "usage"
	recordSessionClose = "session_close"

	rolePublish   = "publish"
	roleSubscribe = "subscribe"
	roleBoth      = "both"
)

// usageRecord is one session record as sent: a session's open, its
// cumulative bytes, or its close with the final totals. Bytes are named from
// the relay's point of view: what it received is ingress, what it sent is
// egress.
type usageRecord struct {
	Type      string           `json:"type"`
	SessionID string           `json:"session_id"`
	KID       string           `json:"kid"`
	Role      string           `json:"role"`
	JTI       string           `json:"jti,omitempty"`
	Metrics   map[string]int64 `json:"metrics,omitempty"`
	Reason    string           `json:"reason,omitempty"`
	TS        string           `json:"ts"`
}

// usageSession is what the reporter remembers of a verified session.
type usageSession struct {
	kid     string
	jti     string
	role    string
	expires time.Time
}

// usageReporter sends session records to a usage URL (QUMO_USAGE_URL): an
// open when a session is admitted, its cumulative bytes at each revalidate,
// and a close with the final totals. Records wait in memory between sends and
// through an outage; usage is coalesced to the latest report per session,
// since each is cumulative, and a receiver can take a resent record without
// counting it twice.
type usageReporter struct {
	url    string
	token  string
	client *http.Client
	now    func() time.Time

	mu       sync.Mutex
	sessions map[string]usageSession
	events   []usageRecord          // opens and closes, in order
	usage    map[string]usageRecord // latest cumulative report per session
}

func newUsageReporter(rawURL, bearer string) (*usageReporter, error) {
	if err := checkURL(rawURL); err != nil {
		return nil, err
	}
	return &usageReporter{
		url:      rawURL,
		token:    bearer,
		client:   noRedirectClient(usageSendTimeout),
		now:      time.Now,
		sessions: map[string]usageSession{},
		usage:    map[string]usageRecord{},
	}, nil
}

// open records a session admitted at connect.
func (r *usageReporter) open(id string, s usageSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[id] = s
	r.appendEvent(r.record(recordSessionOpen, id, s))
}

// reportUsage records a session's cumulative bytes, if it knows the session.
func (r *usageReporter) reportUsage(id string, b Bytes) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return
	}
	rec := r.record(recordUsage, id, s)
	rec.Metrics = usageMetrics(b)
	r.usage[id] = rec
}

// close records a session's end with its final totals and forgets it. It
// reports false for a session it doesn't know.
func (r *usageReporter) close(id string, b Bytes, reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return false
	}
	delete(r.sessions, id)
	delete(r.usage, id) // the close carries totals at least as large
	rec := r.record(recordSessionClose, id, s)
	rec.Metrics = usageMetrics(b)
	rec.Reason = reason
	r.appendEvent(rec)
	return true
}

func (r *usageReporter) record(typ, id string, s usageSession) usageRecord {
	return usageRecord{Type: typ, SessionID: id, KID: s.kid, Role: s.role, JTI: s.jti, TS: r.now().UTC().Format(time.RFC3339)}
}

// appendEvent queues an open or close, dropping the oldest past the cap.
// The caller holds mu.
func (r *usageReporter) appendEvent(rec usageRecord) {
	r.events = append(r.events, rec)
	if over := len(r.events) - maxPendingEvents; over > 0 {
		slog.Warn("relay: usage backlog full; dropping the oldest session events", "dropped", over)
		r.events = r.events[over:]
	}
}

func usageMetrics(b Bytes) map[string]int64 {
	return map[string]int64{"gateway.ingress_bytes": clampInt64(b.Received), "gateway.egress_bytes": clampInt64(b.Sent)}
}

func clampInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// run sends pending records every usageFlushInterval until ctx ends, then
// once more.
func (r *usageReporter) run(ctx context.Context) {
	ticker := time.NewTicker(usageFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			last, cancel := context.WithTimeout(context.WithoutCancel(ctx), usageSendTimeout)
			defer cancel()
			if err := r.flush(last); err != nil {
				slog.Error("relay: last usage send failed; records lost", "error", err)
			}
			return
		case <-ticker.C:
			r.forgetExpired()
			if err := r.flush(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("relay: usage send failed; will retry", "error", err)
			}
		}
	}
}

// forgetExpired drops sessions well past their expiry whose end never came.
func (r *usageReporter) forgetExpired() {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := r.now().Add(-sessionGrace)
	for id, s := range r.sessions {
		if !s.expires.IsZero() && s.expires.Before(cutoff) {
			delete(r.sessions, id)
		}
	}
}

// errMalformed is a 400 or 422 answer to a usage batch: the receiver can't
// read it, so resending it can't succeed.
var errMalformed = errors.New("usage batch malformed")

// flush sends every pending record: opens, then usage, then closes, in
// batches of at most maxBatchRecords. A batch the receiver can't read (400,
// 422) is dropped, so it can't block later records. Any other failure, a
// 401 or 413 included, stops the flush and puts the unsent records back,
// unless newer ones replaced them, to try again next time.
func (r *usageReporter) flush(ctx context.Context) error {
	r.mu.Lock()
	events, usage := r.events, r.usage
	r.events, r.usage = nil, map[string]usageRecord{}
	r.mu.Unlock()
	if len(events) == 0 && len(usage) == 0 {
		return nil
	}

	all := make([]usageRecord, 0, len(events)+len(usage))
	for _, e := range events {
		if e.Type == recordSessionOpen {
			all = append(all, e)
		}
	}
	for _, u := range usage {
		all = append(all, u)
	}
	for _, e := range events {
		if e.Type == recordSessionClose {
			all = append(all, e)
		}
	}

	for start := 0; start < len(all); start += maxBatchRecords {
		batch := all[start:min(start+maxBatchRecords, len(all))]
		err := r.send(ctx, batch)
		switch {
		case err == nil:
		case errors.Is(err, errMalformed):
			slog.Error("relay: usage batch refused as malformed; dropped", "records", len(batch), "error", err)
		default:
			r.requeue(all[start:])
			return err
		}
	}
	return nil
}

// requeue puts unsent records back before anything recorded since: opens
// and closes in order, and a usage record only if its session is still live
// and has no newer one.
func (r *usageReporter) requeue(unsent []usageRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var events []usageRecord
	for _, rec := range unsent {
		if rec.Type != recordUsage {
			events = append(events, rec)
			continue
		}
		if _, newer := r.usage[rec.SessionID]; newer {
			continue
		}
		if _, live := r.sessions[rec.SessionID]; live {
			r.usage[rec.SessionID] = rec
		}
	}
	r.events = append(events, r.events...)
	if over := len(r.events) - maxPendingEvents; over > 0 {
		slog.Warn("relay: usage backlog full; dropping the oldest session events", "dropped", over)
		r.events = r.events[over:]
	}
}

func (r *usageReporter) send(ctx context.Context, batch []usageRecord) error {
	body, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("encode usage: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()                       // not actionable: drained below
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody)) // not actionable: drained to reuse the connection
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: status %d", errMalformed, resp.StatusCode)
	default:
		return fmt.Errorf("status %d", resp.StatusCode)
	}
}
