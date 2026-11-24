// Package meter reports usage records (package usage) to a usage URL
// (QUMO_USAGE_URL), for any qumo server that meters what its tenants use.
//
// A server opens a session for each thing it meters under a tenant's key,
// such as a relay session or an ingested stream, reports its cumulative
// metrics while it lasts, and closes it with its final totals. Usage that
// isn't worth a record per session, such as a relay's viewers, goes in one
// running total per key for the run of the server. Every record is
// cumulative, so a receiver can take a resent one without counting it twice.
//
// Records wait in memory between sends, every 10 s, and through an outage; a
// session's usage is coalesced to its latest report.
package meter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/qumo-dev/qumo/internal/reqsign"
	"github.com/qumo-dev/qumo/usage"
)

const (
	// flushInterval is how often pending records are sent.
	flushInterval = 10 * time.Second
	// sendTimeout bounds one send, and the last send at shutdown.
	sendTimeout = 10 * time.Second
	// maxBatchRecords bounds one POST, so a backlog after an outage goes out
	// in pieces a receiver's body limit accepts.
	maxBatchRecords = 500
	// maxPendingEvents caps the opens and closes waiting while the receiver
	// can't be reached; the oldest are dropped past it. Usage needs no cap:
	// it is one cumulative record per live session.
	maxPendingEvents = 50_000
	// maxBody bounds how much of a reply's body is drained.
	maxBody = 64 << 10
)

// Config is where a Reporter sends its records.
type Config struct {
	// Name says who reports, in the log: "relay", "ingest".
	Name string
	// URL receives the records: https, or http to a loopback host.
	URL string
	// Token, when set, is sent as a bearer token.
	Token string
	// Signer, when set, signs each request with the server's identity.
	Signer *reqsign.Signer
}

// total is one key's running total for this run, and what the receiver has
// acknowledged of it.
type total struct {
	metrics  map[string]int64
	reported map[string]int64
	sent     bool // reported at least once
}

// A Reporter sends usage records to a usage URL. Its methods are safe for
// concurrent use.
type Reporter struct {
	name   string
	url    string
	token  string
	signer *reqsign.Signer // nil: requests are not signed
	client *http.Client
	// refusals logs the receiver's 401s, the first of a run only.
	refusals reqsign.Refusals
	// runID names this run of the server in its totals: a restarted server
	// starts new totals rather than lowering old ones.
	runID string

	mu      sync.Mutex
	live    map[string]string       // open session id -> kid, until closed or forgotten
	events  []usage.Record          // opens and closes, in order
	latest  map[string]usage.Record // latest cumulative report per session
	totals  map[string]*total       // by kid
	failing bool                    // the last send failed
}

// New returns a Reporter sending to cfg.URL. Run sends its records.
func New(cfg Config) (*Reporter, error) {
	if err := reqsign.CheckURL(cfg.URL); err != nil {
		return nil, err
	}
	return &Reporter{
		name:   cfg.Name,
		url:    cfg.URL,
		token:  cfg.Token,
		signer: cfg.Signer,
		client: reqsign.NoRedirectClient(sendTimeout),
		runID:  rand.Text(),
		live:   map[string]string{},
		latest: map[string]usage.Record{},
		totals: map[string]*total{},
	}, nil
}

// Open records that session id began under key kid: a session_open.
func (r *Reporter) Open(id, kid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.live[id] = kid
	r.appendEvent(r.record(usage.SessionOpen, id, kid))
}

// Report records the cumulative metrics of session id so far: a usage
// record, replacing any not yet sent. A session not open is ignored.
func (r *Reporter) Report(id string, metrics map[string]int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kid, ok := r.live[id]
	if !ok {
		return
	}
	rec := r.record(usage.Usage, id, kid)
	rec.Metrics = maps.Clone(metrics)
	r.latest[id] = rec
}

// Close records the end of session id with its final totals and the reason
// it ended: a session_close, which supersedes its pending usage. A session
// not open is ignored.
func (r *Reporter) Close(id string, metrics map[string]int64, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kid, ok := r.live[id]
	if !ok {
		return
	}
	delete(r.live, id)
	delete(r.latest, id) // the close carries totals at least as large
	rec := r.record(usage.SessionClose, id, kid)
	rec.Metrics = maps.Clone(metrics)
	rec.Reason = reason
	r.appendEvent(rec)
}

// Forget drops session id without a close, for one whose end never came. Its
// usage not yet sent still goes, but is not sent again after a failure.
func (r *Reporter) Forget(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.live, id)
}

// Total records the running total of key kid for this run: a total record
// (usage.Viewers), sent whenever it differs from what the receiver last
// acknowledged.
func (r *Reporter) Total(kid string, metrics map[string]int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.totals[kid]
	if t == nil {
		t = &total{}
		r.totals[kid] = t
	}
	t.metrics = maps.Clone(metrics)
}

func (r *Reporter) record(typ usage.Type, id, kid string) usage.Record {
	return usage.Record{Type: typ, SessionID: id, KID: kid, TS: r.timestamp()}
}

// timestamp is a record's ts: now, to the second.
func (r *Reporter) timestamp() time.Time {
	return time.Now().UTC().Truncate(time.Second)
}

// appendEvent queues an open or close, dropping the oldest past the cap.
// The caller holds mu.
func (r *Reporter) appendEvent(rec usage.Record) {
	r.events = append(r.events, rec)
	r.capEvents()
}

// capEvents drops the oldest events past maxPendingEvents. The caller holds
// mu.
func (r *Reporter) capEvents() {
	if over := len(r.events) - maxPendingEvents; over > 0 {
		slog.Warn(r.name+": usage backlog full; dropping the oldest session events", "dropped", over)
		r.events = r.events[over:]
	}
}

// Run sends pending records every 10 s until ctx ends, then once more.
func (r *Reporter) Run(ctx context.Context) {
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			last, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
			defer cancel()
			if err := r.Flush(last); err != nil {
				slog.Error(r.name+": last usage send failed; records lost", "error", err)
			}
			return
		case <-ticker.C:
			r.logOutcome(ctx, r.Flush(ctx))
		}
	}
}

// logOutcome logs a send's first failure as a warning, repeats quietly, and
// the recovery as info: during an outage the server retries every few
// seconds, and one line says so.
func (r *Reporter) logOutcome(ctx context.Context, err error) {
	r.mu.Lock()
	wasFailing := r.failing
	r.failing = err != nil
	r.mu.Unlock()
	switch {
	case err != nil && ctx.Err() != nil:
	case err != nil && !wasFailing:
		slog.Warn(r.name+": usage send failed; keeping the records and retrying", "error", err)
	case err != nil:
		slog.Debug(r.name+": usage send still failing", "error", err)
	case wasFailing:
		slog.Info(r.name + ": usage send recovered")
	}
}

// errMalformed is a 400 or 422 answer to a batch: the receiver can't read
// it, so resending it can't succeed.
var errMalformed = errors.New("usage batch malformed")

// Flush sends every pending record: opens, then per-session usage, then the
// totals that changed, then closes, in batches of at most 500. A batch the
// receiver can't read (400, 422) is dropped, so it can't block later
// records. Any other failure, a 401 or 413 included, stops the flush and puts
// the unsent records back, unless newer ones replaced them, to try again
// next time.
func (r *Reporter) Flush(ctx context.Context) error {
	r.mu.Lock()
	events, latest := r.events, r.latest
	r.events, r.latest = nil, map[string]usage.Record{}
	// A total is sent when it has changed since the receiver last
	// acknowledged it. It stays pending until then, so it needs no requeue.
	sending := map[string]map[string]int64{} // by kid
	var totals []usage.Record
	for kid, t := range r.totals {
		if t.sent && maps.Equal(t.metrics, t.reported) {
			continue
		}
		totals = append(totals, usage.Record{
			Type: usage.Viewers, Run: r.runID, KID: kid, Metrics: maps.Clone(t.metrics), TS: r.timestamp(),
		})
		sending[kid] = t.metrics
	}
	r.mu.Unlock()
	if len(events) == 0 && len(latest) == 0 && len(totals) == 0 {
		return nil
	}

	all := make([]usage.Record, 0, len(events)+len(latest)+len(totals))
	for _, e := range events {
		if e.Type == usage.SessionOpen {
			all = append(all, e)
		}
	}
	for _, u := range latest {
		all = append(all, u)
	}
	all = append(all, totals...)
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
			r.acknowledge(batch, sending)
		case errors.Is(err, errMalformed):
			slog.Error(r.name+": usage batch refused as malformed; dropped", "records", len(batch), "error", err)
			// Not resent as it is; a total goes again when it changes.
			r.acknowledge(batch, sending)
		default:
			r.requeue(all[start:])
			return err
		}
	}
	return nil
}

// acknowledge records the totals in a sent batch, by kid in sending, as
// reported.
func (r *Reporter) acknowledge(batch []usage.Record, sending map[string]map[string]int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range batch {
		if rec.Type != usage.Viewers {
			continue
		}
		if t := r.totals[rec.KID]; t != nil {
			t.reported, t.sent = sending[rec.KID], true
		}
	}
}

// requeue puts unsent records back before anything recorded since: opens
// and closes in order, and a session's usage record only if the session is
// still live and has no newer one. Totals aren't put back: they stay pending
// until acknowledged.
func (r *Reporter) requeue(unsent []usage.Record) {
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
		if _, live := r.live[rec.SessionID]; live {
			r.latest[rec.SessionID] = rec
		}
	}
	r.events = append(events, r.events...)
	r.capEvents()
}

func (r *Reporter) send(ctx context.Context, batch []usage.Record) error {
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
	// Signed at each send: a batch sent again after a failure carries a
	// fresh timestamp.
	if err := r.signer.Sign(req, body); err != nil {
		return fmt.Errorf("usage report: %w", err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()                       // not actionable: drained below
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody)) // not actionable: drained to reuse the connection
	r.refusals.Observe(r.url, resp)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: status %d", errMalformed, resp.StatusCode)
	default:
		return fmt.Errorf("status %d", resp.StatusCode)
	}
}
