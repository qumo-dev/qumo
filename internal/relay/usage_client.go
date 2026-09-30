package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// UsageEvent is a single cumulative usage record for a broadcast session.
// All byte values are totals since session start; the server diffs consecutive reports.
type UsageEvent struct {
	BroadcastSessionID string `json:"broadcast_session_id"`
	// KeyID is the kid of the signing key that admitted the session.
	KeyID   string           `json:"kid"`
	Metrics map[string]int64 `json:"metrics"`
	Ts      string           `json:"ts"` // RFC3339
}

// usageClient reports session usage to the qumo control plane.
//
// Configuration is read from environment variables:
//
//	QUMO_CREDENTIAL_URL  - base URL of the control plane
//	QUMO_RELAY_TOKEN     - shared bearer token (must match the server's config)
type usageClient struct {
	baseURL    string
	authToken  string
	httpClient *http.Client
}

// newUsageClient creates a usageClient from environment variables.
// Returns nil when QUMO_CREDENTIAL_URL is not set (usage is not reported).
func newUsageClient() *usageClient {
	baseURL := normalizeCredentialURL(os.Getenv("QUMO_CREDENTIAL_URL"))
	if baseURL == "" {
		return nil
	}

	authToken := os.Getenv("QUMO_RELAY_TOKEN")
	if authToken == "" {
		slog.Warn("QUMO_CREDENTIAL_URL is set but QUMO_RELAY_TOKEN is empty")
	}

	slog.Info("usage client configured", "url", baseURL)
	return &usageClient{
		baseURL:   baseURL,
		authToken: authToken,
		// Uses http.DefaultTransport which trusts the system CA pool.
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// normalizeCredentialURL adds https:// when no scheme is given and drops a
// trailing slash.
func normalizeCredentialURL(u string) string {
	if u == "" {
		return ""
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "https://" + u
	}
	return strings.TrimRight(u, "/")
}

// ReportUsage POSTs a batch of cumulative usage events to the control plane.
func (c *usageClient) ReportUsage(ctx context.Context, events []UsageEvent) error {
	if len(events) == 0 {
		return nil
	}

	body, err := json.Marshal(events)
	if err != nil {
		return fmt.Errorf("usage: marshal events: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/usage/events", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("usage: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("usage: report: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("usage: events returned HTTP %d", resp.StatusCode)
	}
	return nil
}
