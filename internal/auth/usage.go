package auth

import (
	"bytes"
	"context"
	"crypto/rand"
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
	Metrics   map[string]int64 `json:"metrics,omitempty"`
	Reason    string           `json:"reason,omitempty"`
	TS        string           `json:"ts"`
}

// usageSession is what the reporter remembers of a verified session.
type usageSession struct {
	kid     string
	role    string
	expires time.Time
	// counted is how much of a subscribe-only session's bytes its key's
	// viewer total already holds.
	counted Bytes
}

// viewerTotal is the bytes of every subscribe-only session of one key since
// the relay started, and how much of it the receiver has acknowledged.
type viewerTotal struct {
	total    Bytes
	reported Bytes
	sent     bool // reported at least once
}

// usageReporter sends usage records to a usage URL (QUMO_USAGE_URL).
//
// A session that may publish is reported by itself: an open when it is
// admitted, its cumulative bytes at each revalidate, and a close with the
// final totals.
//
// Sessions that only subscribe are reported together. Viewers outnumber
// publishers by orders of magnitude, and a record per viewer every 30 s would
// make the receiver's load grow with the audience. Their bytes are added up
// per key into one running total since the relay started, sent as a usage
// record whose session id names this run of the relay and the key
// ("viewers.<run>.<kid>"). No open or close is sent for them.
//
// Every record is cumulative, so a receiver can take a resent one without
// counting it twice. Records wait in memory between sends and through an
// outage; per-session usage is coalesced to its latest report.
type usageReporter struct {
	url    string
	token  string
	client *http.Client
	now    func() time.Time
	// run names this run of the relay in viewer totals' session ids: a
	// restarted relay starts new totals rather than lowering old ones.
	runID string

	mu       sync.Mutex
	sessions map[string]usageSession
	events   []usageRecord           // opens and closes, in order
	usage    map[string]usageRecord  // latest cumulative report per session
	viewers  map[string]*viewerTotal // by kid
	failing  bool                    // the last send failed
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
		runID:    rand.Text(),
		sessions: map[string]usageSession{},
		usage:    map[string]usageRecord{},
		viewers:  map[string]*viewerTotal{},
	}, nil
}

// open records a session admitted at connect.
func (r *usageReporter) open(id string, s usageSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[id] = s
	if s.role != roleSubscribe {
		r.appendEvent(r.record(recordSessionOpen, id, s))
	}
}

// reportUsage records a session's cumulative bytes, if it knows the session.
func (r *usageReporter) reportUsage(id string, b Bytes) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return
	}
	if s.role == roleSubscribe {
		r.sessions[id] = r.countViewer(s, b)
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
	if s.role == roleSubscribe {
		r.countViewer(s, b)
		return true
	}
	delete(r.usage, id) // the close carries totals at least as large
	rec := r.record(recordSessionClose, id, s)
	rec.Metrics = usageMetrics(b)
	rec.Reason = reason
	r.appendEvent(rec)
	return true
}

// countViewer adds what a subscribe-only session moved since it was last
// counted to its key's viewer total, and returns the session with that
// recorded. The caller holds mu.
func (r *usageReporter) countViewer(s usageSession, b Bytes) usageSession {
	t := r.viewers[s.kid]
	if t == nil {
		t = &viewerTotal{}
		r.viewers[s.kid] = t
	}
	if b.Sent > s.counted.Sent {
		t.total.Sent += b.Sent - s.counted.Sent
		s.counted.Sent = b.Sent
	}
	if b.Received > s.counted.Received {
		t.total.Received += b.Received - s.counted.Received
		s.counted.Received = b.Received
	}
	return s
}

// viewerSessionID is the session id a key's viewer total is reported under.
func (r *usageReporter) viewerSessionID(kid string) string {
	return "viewers." + r.runID + "." + kid
}

func (r *usageReporter) record(typ, id string, s usageSession) usageRecord {
	return usageRecord{Type: typ, SessionID: id, KID: s.kid, Role: s.role, TS: r.now().UTC().Format(time.RFC3339)}
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
			r.logOutcome(ctx, r.flush(ctx))
		}
	}
}

// logOutcome logs a send's first failure as a warning, repeats quietly, and
// the recovery as info: during an outage the relay retries every few
// seconds, and one line says so.
func (r *usageReporter) logOutcome(ctx context.Context, err error) {
	r.mu.Lock()
	wasFailing := r.failing
	r.failing = err != nil
	r.mu.Unlock()
	switch {
	case err != nil && ctx.Err() != nil:
	case err != nil && !wasFailing:
		slog.Warn("relay: usage send failed; keeping the records and retrying", "error", err)
	case err != nil:
		slog.Debug("relay: usage send still failing", "error", err)
	case wasFailing:
		slog.Info("relay: usage send recovered")
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

// flush sends every pending record: opens, then per-session usage, then the
// viewer totals that changed, then closes, in batches of at most
// maxBatchRecords. A batch the receiver can't read (400, 422) is dropped, so
// it can't block later records. Any other failure, a 401 or 413 included,
// stops the flush and puts the unsent records back, unless newer ones
// replaced them, to try again next time.
func (r *usageReporter) flush(ctx context.Context) error {
	r.mu.Lock()
	events, usage := r.events, r.usage
	r.events, r.usage = nil, map[string]usageRecord{}
	// A viewer total is sent when it has changed since the receiver last
	// acknowledged it. It stays pending until then, so it needs no requeue.
	totals := map[string]Bytes{} // by the record's session id
	kids := map[string]string{}
	var viewers []usageRecord
	for kid, t := range r.viewers {
		if t.sent && t.total == t.reported {
			continue
		}
		id := r.viewerSessionID(kid)
		rec := r.record(recordUsage, id, usageSession{kid: kid, role: roleSubscribe})
		rec.Metrics = usageMetrics(t.total)
		viewers = append(viewers, rec)
		totals[id], kids[id] = t.total, kid
	}
	r.mu.Unlock()
	if len(events) == 0 && len(usage) == 0 && len(viewers) == 0 {
		return nil
	}

	all := make([]usageRecord, 0, len(events)+len(usage)+len(viewers))
	for _, e := range events {
		if e.Type == recordSessionOpen {
			all = append(all, e)
		}
	}
	for _, u := range usage {
		all = append(all, u)
	}
	all = append(all, viewers...)
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
			r.acknowledge(batch, totals, kids)
		case errors.Is(err, errMalformed):
			slog.Error("relay: usage batch refused as malformed; dropped", "records", len(batch), "error", err)
			// Not resent as it is; a viewer total goes again when it grows.
			r.acknowledge(batch, totals, kids)
		default:
			r.requeue(all[start:], kids)
			return err
		}
	}
	return nil
}

// acknowledge records the viewer totals in a sent batch as reported.
func (r *usageReporter) acknowledge(batch []usageRecord, totals map[string]Bytes, kids map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range batch {
		kid, ok := kids[rec.SessionID]
		if !ok {
			continue
		}
		if t := r.viewers[kid]; t != nil {
			t.reported, t.sent = totals[rec.SessionID], true
		}
	}
}

// requeue puts unsent records back before anything recorded since: opens
// and closes in order, and a session's usage record only if the session is
// still live and has no newer one. Viewer totals (viewerIDs) aren't put
// back: they stay pending until acknowledged.
func (r *usageReporter) requeue(unsent []usageRecord, viewerIDs map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var events []usageRecord
	for _, rec := range unsent {
		if _, viewers := viewerIDs[rec.SessionID]; viewers {
			continue
		}
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
