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
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const credentialMaxBody = 1 << 20 // 1 MB

// IntrospectResult holds the validated details from a successful credential introspection.
type IntrospectResult struct {
	TokenID     string
	ProjectID   string
	TenantID    string
	APIKeyID    string
	Environment string
}

type cachedCredential struct {
	result  IntrospectResult
	expires time.Time
}

// UsageEvent is a single cumulative usage record for a broadcast session.
// All byte values are totals since session start; the server diffs consecutive reports.
type UsageEvent struct {
	BroadcastSessionID string           `json:"broadcast_session_id"`
	OwnerTokenID       string           `json:"owner_token_id"`
	Metrics            map[string]int64 `json:"metrics"`
	Ts                 string           `json:"ts"` // RFC3339
}

// CredentialClient validates publisher credentials and reports session usage to
// the qumo control plane.
//
// Configuration is read from environment variables:
//
//	QUMO_CREDENTIAL_URL  - base URL of the credential server
//	QUMO_RELAY_TOKEN     - shared bearer token (must match the server's config)
type CredentialClient struct {
	baseURL    string
	authToken  string
	httpClient *http.Client

	cacheMu sync.Mutex
	cache   map[string]cachedCredential
	sfGroup singleflight.Group // deduplicates concurrent requests for the same JWT
}

// NewCredentialClient creates a CredentialClient from environment variables.
// Returns nil when QUMO_CREDENTIAL_URL is not set (credential auth and metering disabled).
func NewCredentialClient() *CredentialClient {
	baseURL := os.Getenv("QUMO_CREDENTIAL_URL")
	if baseURL == "" {
		return nil
	}
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		baseURL = "https://" + baseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	authToken := os.Getenv("QUMO_RELAY_TOKEN")
	if authToken == "" {
		slog.Warn("QUMO_CREDENTIAL_URL is set but QUMO_RELAY_TOKEN is empty")
	}

	slog.Info("credential client configured", "url", baseURL)
	return &CredentialClient{
		baseURL:   baseURL,
		authToken: authToken,
		// Uses http.DefaultTransport which trusts the system CA pool.
		// If the server uses a custom CA, configure it via HTTPS_PROXY or
		// extend this to accept a *tls.Config when that becomes necessary.
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		cache: make(map[string]cachedCredential),
	}
}

// Introspect validates a publisher JWT for announcing broadcastPath against the
// credential introspection endpoint. The relay only forwards the path it saw in
// the ANNOUNCE; whether the credential covers it is the control plane's
// decision.
// Returns (nil, nil) when the token is present but the server reports valid:false.
// Results are cached per (JWT, path) until the server-supplied revalidate_after
// time: a credential valid for one path says nothing about another.
// Concurrent calls for the same (JWT, path) are coalesced into a single HTTP request.
func (c *CredentialClient) Introspect(ctx context.Context, jwt, broadcastPath string) (*IntrospectResult, error) {
	key := introspectCacheKey(jwt, broadcastPath)

	// Fast path: cache hit.
	c.cacheMu.Lock()
	if cached, ok := c.cache[key]; ok && time.Now().Before(cached.expires) {
		result := cached.result
		c.cacheMu.Unlock()
		return &result, nil
	}
	c.cacheMu.Unlock()

	// Coalesce concurrent requests for the same JWT into one HTTP round-trip.
	// The shared return value is (*IntrospectResult)(nil) when valid:false.
	// Note: the singleflight function captures ctx from the first caller. If
	// that context is cancelled (e.g. authCtx times out), all coalesced callers
	// receive the same cancellation error. This is correct for auth: a timeout
	// on any one caller should reject all publishers waiting on the same JWT.
	v, err, _ := c.sfGroup.Do(key, func() (any, error) {
		// Re-check cache: another goroutine may have populated it while this one
		// was waiting for the singleflight lock.
		c.cacheMu.Lock()
		if cached, ok := c.cache[key]; ok && time.Now().Before(cached.expires) {
			result := cached.result
			c.cacheMu.Unlock()
			return &result, nil
		}
		c.cacheMu.Unlock()

		body, err := json.Marshal(map[string]string{"token": jwt, "broadcast_path": broadcastPath})
		if err != nil {
			return nil, fmt.Errorf("credential: marshal introspect request: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.baseURL+"/v1/credentials/introspect", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("credential: create introspect request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if c.authToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.authToken)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("credential: introspect: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, resp.Body)
			return nil, fmt.Errorf("credential: introspect returned HTTP %d", resp.StatusCode)
		}

		var raw struct {
			Valid           bool   `json:"valid"`
			TokenID         string `json:"token_id"`
			ProjectID       string `json:"project_id"`
			TenantID        string `json:"tenant_id"`
			APIKeyID        string `json:"api_key_id"`
			Environment     string `json:"environment"`
			RevalidateAfter string `json:"revalidate_after"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, credentialMaxBody)).Decode(&raw); err != nil {
			return nil, fmt.Errorf("credential: decode introspect response: %w", err)
		}

		if !raw.Valid {
			// Negative results are not cached — see issue #90 for the tradeoffs
			// around TTL selection, operator visibility, and backend alignment.
			return (*IntrospectResult)(nil), nil // valid:false, not an error
		}

		expires, err := time.Parse(time.RFC3339, raw.RevalidateAfter)
		if err != nil || expires.IsZero() || !expires.After(time.Now()) {
			expires = time.Now().Add(5 * time.Minute) // safe fallback
		}

		result := IntrospectResult{
			TokenID:     raw.TokenID,
			ProjectID:   raw.ProjectID,
			TenantID:    raw.TenantID,
			APIKeyID:    raw.APIKeyID,
			Environment: raw.Environment,
		}

		c.cacheMu.Lock()
		c.cache[key] = cachedCredential{result: result, expires: expires}
		// Sweep expired entries to prevent unbounded cache growth.
		now := time.Now()
		for k, v := range c.cache {
			if now.After(v.expires) {
				delete(c.cache, k)
			}
		}
		c.cacheMu.Unlock()

		return &result, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*IntrospectResult), nil
}

// introspectCacheKey keys the cache and singleflight group by credential and
// path together. NUL cannot appear in a JWT or a MoQ broadcast path.
func introspectCacheKey(jwt, broadcastPath string) string {
	return jwt + "\x00" + broadcastPath
}

// ReportUsage POSTs a batch of cumulative usage events to the credential server.
func (c *CredentialClient) ReportUsage(ctx context.Context, events []UsageEvent) error {
	if len(events) == 0 {
		return nil
	}

	body, err := json.Marshal(events)
	if err != nil {
		return fmt.Errorf("credential: marshal usage events: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/usage/events", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("credential: create usage request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("credential: report usage: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("credential: usage events returned HTTP %d", resp.StatusCode)
	}
	return nil
}
