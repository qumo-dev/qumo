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

	"github.com/qumo-dev/qumo/usage"
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
	// sessionGrace is how long a session is remembered after it was last
	// heard of, at connect or a revalidate, for an end that arrives late.
	sessionGrace = 5 * time.Minute
)

// usageSession is what the reporter remembers of a verified session.
type usageSession struct {
	kid string
	// publishes is a session that may publish a broadcast, reported by
	// itself; every other session is a viewer, in its key's viewer total.
	publishes bool
	// seen is when the session was last heard of, at connect or a
	// revalidate: one whose end never came is forgotten once it stops being
	// revalidated.
	seen time.Time
	// counted is how much of a viewer session's bytes its key's viewer total
	// already holds. A viewer that became a publisher keeps it, and its
	// usage records carry only what it moved beyond it.
	counted Bytes
}

// viewerTotal is the bytes of every viewer session of one key since the relay
// started, and how much of it the receiver has acknowledged.
type viewerTotal struct {
	total    Bytes
	reported Bytes
	sent     bool // reported at least once
}

// usageReporter sends usage records (package usage) to a usage URL
// (QUMO_USAGE_URL).
//
// A session that may publish is reported by itself: an open when it is
// admitted, its cumulative bytes at each revalidate, and a close with the
// final totals. Every other session is a viewer, added up per key into one
// running total for this run of the relay, sent as a viewers record. No open
// or close is sent for viewers.
//
// Every record is cumulative, so a receiver can take a resent one without
// counting it twice. Records wait in memory between sends and through an
// outage; per-session usage is coalesced to its latest report.
type usageReporter struct {
	url    string
	token  string
	client *http.Client
	now    func() time.Time
	// run names this run of the relay in its viewers records: a restarted
	// relay starts new totals rather than lowering old ones.
	runID string

	mu       sync.Mutex
	sessions map[string]usageSession
	events   []usage.Record          // opens and closes, in order
	latest   map[string]usage.Record // latest cumulative report per session
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
		latest:   map[string]usage.Record{},
		viewers:  map[string]*viewerTotal{},
	}, nil
}

// open records a session admitted at connect.
func (r *usageReporter) open(id string, s usageSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s.seen = r.now()
	r.sessions[id] = s
	if s.publishes {
		r.appendEvent(r.record(usage.SessionOpen, id, s))
	}
}

// publishing records that a viewer session may now publish, such as by a
// ticket it presented: it is reported by itself from now on, opening with a
// session_open. What it moved as a viewer up to its last revalidate stays in
// its key's viewer total; what it moved since is reported as its usage. A session it doesn't know, or that publishes already, is left as it
// is.
func (r *usageReporter) publishing(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok || s.publishes {
		return
	}
	s.publishes = true
	r.sessions[id] = s
	r.appendEvent(r.record(usage.SessionOpen, id, s))
}

// revalidated records a session's cumulative bytes at a revalidate and, when
// live is set, that the session is still live. A session it doesn't know is
// left unknown.
func (r *usageReporter) revalidated(id string, b Bytes, live bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return
	}
	if live {
		s.seen = r.now()
	}
	switch {
	case !s.publishes:
		s = r.countViewer(s, b)
	case b.Sent > s.counted.Sent || b.Received > s.counted.Received:
		rec := r.record(usage.Usage, id, s)
		rec.Metrics = usageMetrics(b.since(s.counted))
		r.latest[id] = rec
	}
	r.sessions[id] = s
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
	if !s.publishes {
		r.countViewer(s, b)
		return true
	}
	delete(r.latest, id) // the close carries totals at least as large
	rec := r.record(usage.SessionClose, id, s)
	rec.Metrics = usageMetrics(b.since(s.counted))
	rec.Reason = reason
	r.appendEvent(rec)
	return true
}

// countViewer adds what a viewer session moved since it was last
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

func (r *usageReporter) record(typ usage.Type, id string, s usageSession) usage.Record {
	return usage.Record{Type: typ, SessionID: id, KID: s.kid, TS: r.timestamp()}
}

// timestamp is a record's ts: now, to the second, as the reports have always
// carried it.
func (r *usageReporter) timestamp() time.Time {
	return r.now().UTC().Truncate(time.Second)
}

// appendEvent queues an open or close, dropping the oldest past the cap.
// The caller holds mu.
func (r *usageReporter) appendEvent(rec usage.Record) {
	r.events = append(r.events, rec)
	if over := len(r.events) - maxPendingEvents; over > 0 {
		slog.Warn("relay: usage backlog full; dropping the oldest session events", "dropped", over)
		r.events = r.events[over:]
	}
}

func usageMetrics(b Bytes) map[string]int64 {
	return map[string]int64{usage.IngressBytes: clampInt64(b.Received), usage.EgressBytes: clampInt64(b.Sent)}
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
			r.forgetStale()
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

// forgetStale drops sessions whose end never came: those not heard of for
// sessionGrace.
func (r *usageReporter) forgetStale() {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := r.now().Add(-sessionGrace)
	for id, s := range r.sessions {
		if s.seen.Before(cutoff) {
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
	events, latest := r.events, r.latest
	r.events, r.latest = nil, map[string]usage.Record{}
	// A viewer total is sent when it has changed since the receiver last
	// acknowledged it. It stays pending until then, so it needs no requeue.
	totals := map[string]Bytes{} // by kid
	var viewers []usage.Record
	for kid, t := range r.viewers {
		if t.sent && t.total == t.reported {
			continue
		}
		viewers = append(viewers, usage.Record{
			Type: usage.Viewers, Run: r.runID, KID: kid, Metrics: usageMetrics(t.total), TS: r.timestamp(),
		})
		totals[kid] = t.total
	}
	r.mu.Unlock()
	if len(events) == 0 && len(latest) == 0 && len(viewers) == 0 {
		return nil
	}

	all := make([]usage.Record, 0, len(events)+len(latest)+len(viewers))
	for _, e := range events {
		if e.Type == usage.SessionOpen {
			all = append(all, e)
		}
	}
	for _, u := range latest {
		all = append(all, u)
	}
	all = append(all, viewers...)
	for _, e := range events {
		if e.Type == usage.SessionClose {
			all = append(all, e)
		}
	}

	for start := 0; start < len(all); start += maxBatchRecords {
		batch := all[start:min(start+maxBatchRecords, len(all))]
		err := r.send(ctx, batch)
		switch {
		case err == nil:
			r.acknowledge(batch, totals)
		case errors.Is(err, errMalformed):
			slog.Error("relay: usage batch refused as malformed; dropped", "records", len(batch), "error", err)
			// Not resent as it is; a viewer total goes again when it grows.
			r.acknowledge(batch, totals)
		default:
			r.requeue(all[start:])
			return err
		}
	}
	return nil
}

// acknowledge records the viewer totals in a sent batch, by kid in totals, as
// reported.
func (r *usageReporter) acknowledge(batch []usage.Record, totals map[string]Bytes) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range batch {
		if rec.Type != usage.Viewers {
			continue
		}
		if t := r.viewers[rec.KID]; t != nil {
			t.reported, t.sent = totals[rec.KID], true
		}
	}
}

// requeue puts unsent records back before anything recorded since: opens
// and closes in order, and a session's usage record only if the session is
// still live and has no newer one. Viewer totals aren't put back: they stay
// pending until acknowledged.
func (r *usageReporter) requeue(unsent []usage.Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var events []usage.Record
	for _, rec := range unsent {
		switch rec.Type {
		case usage.Viewers:
			continue
		case usage.SessionOpen, usage.SessionClose:
			events = append(events, rec)
			continue
		}
		if _, newer := r.latest[rec.SessionID]; newer {
			continue
		}
		if _, live := r.sessions[rec.SessionID]; live {
			r.latest[rec.SessionID] = rec
		}
	}
	r.events = append(events, r.events...)
	if over := len(r.events) - maxPendingEvents; over > 0 {
		slog.Warn("relay: usage backlog full; dropping the oldest session events", "dropped", over)
		r.events = r.events[over:]
	}
}

func (r *usageReporter) send(ctx context.Context, batch []usage.Record) error {
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

// since returns what b moved beyond base, each total at least zero.
func (b Bytes) since(base Bytes) Bytes {
	return Bytes{Sent: b.Sent - min(b.Sent, base.Sent), Received: b.Received - min(b.Received, base.Received)}
}
