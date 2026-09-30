package trust

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const (
	// trustPath is where the control plane serves the trust snapshot.
	trustPath = "/v1/relays/trust"
	// pollInterval is the steady-state poll period; revocation and suspension
	// take effect within one interval.
	pollInterval = 30 * time.Second
	// initialRetryInterval paces polls until the first snapshot loads; no
	// session is admitted before then.
	initialRetryInterval = 5 * time.Second
	// maxSnapshotBytes bounds a snapshot read (ADR 0035 budgets about 3 MB at
	// 10,000 projects).
	maxSnapshotBytes = 32 << 20
)

// Poller keeps a Store current by polling the control plane's trust snapshot
// with If-None-Match.
type Poller struct {
	url    string
	token  string
	client *http.Client
	store  *Store

	etag string
}

// NewPoller returns a Poller that fetches baseURL's trust snapshot into store,
// authenticating with the relay token.
func NewPoller(baseURL, token string, client *http.Client, store *Store) *Poller {
	return &Poller{url: baseURL + trustPath, token: token, client: client, store: store}
}

// URL is the address the snapshot is polled from.
func (p *Poller) URL() string { return p.url }

// Run polls until ctx is done: every initialRetryInterval until the first
// snapshot loads, then every pollInterval. A failed poll keeps the last
// snapshot (fail-static; Store.Key stops admitting once it is too stale).
func (p *Poller) Run(ctx context.Context) {
	for {
		err := p.Poll(ctx)
		if err != nil {
			metricPollFailures.Inc()
			slog.Warn("trust: snapshot poll failed; keeping the last snapshot", "url", p.url, "error", err)
		}
		interval := pollInterval
		if p.store.current.Load() == nil {
			interval = initialRetryInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Poll fetches the snapshot once. An unchanged snapshot (304) only refreshes
// the Store's freshness; a new one replaces it.
func (p *Poller) Poll(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	if p.etag != "" {
		req.Header.Set("If-None-Match", p.etag)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		_, _ = io.Copy(io.Discard, resp.Body)
		if p.store.current.Load() == nil {
			// A 304 before any snapshot loaded means our ETag is stale state;
			// drop it so the next poll fetches the full snapshot.
			p.etag = ""
			return errors.New("304 before any snapshot loaded")
		}
		p.store.markFresh()
		return nil
	case http.StatusOK:
	default:
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("status %d", resp.StatusCode)
	}

	var snap Snapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSnapshotBytes)).Decode(&snap); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	p.store.replace(snap)
	p.etag = resp.Header.Get("ETag")
	return nil
}
