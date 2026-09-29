package credential

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	// jwksPath is where the control plane publishes its public keys.
	jwksPath = "/v1/credentials/jwks"
	// jwksMaxBody bounds the key set a fetch will read.
	jwksMaxBody = 1 << 20

	// jwksRefreshInterval matches the endpoint's Cache-Control max-age.
	jwksRefreshInterval = 5 * time.Minute
	// jwksRetryInterval paces fetches until the first one succeeds; no
	// credential is admitted before then.
	jwksRetryInterval = 10 * time.Second
	// jwksUnknownKIDInterval bounds refetches triggered by an unknown kid, so
	// a stream of forged kids cannot turn into a stream of fetches.
	jwksUnknownKIDInterval = 30 * time.Second
	// jwksMaxStaleness is how long the key set keeps answering after
	// refreshes start failing (fail-static). A public key never expires, so
	// failing closed at once would put the control plane back in the
	// admission path; the cap bounds exposure to a removed, compromised key.
	jwksMaxStaleness = 6 * time.Hour
)

// JWKS holds the control plane's public keys by kid. It fetches at start,
// refreshes on a timer, refetches on an unknown kid (rate-limited), and keeps
// the last good set when a refresh fails, until jwksMaxStaleness.
type JWKS struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu               sync.Mutex
	keys             map[string]ed25519.PublicKey
	lastSuccess      time.Time
	lastUnknownFetch time.Time
}

var _ Keys = (*JWKS)(nil)

// NewJWKS returns the key set of the control plane at baseURL. It holds no
// keys until Run, or a Key call for an unknown kid, fetches them.
func NewJWKS(baseURL string, client *http.Client) *JWKS {
	return &JWKS{url: baseURL + jwksPath, client: client, now: time.Now}
}

// URL is the address the key set is fetched from.
func (j *JWKS) URL() string { return j.url }

// Run fetches until the first success, then refreshes every
// jwksRefreshInterval, until ctx is done.
func (j *JWKS) Run(ctx context.Context) {
	interval := jwksRetryInterval
	for {
		if err := j.refresh(ctx); err != nil {
			msg := "credential: JWKS fetch failed; no keys yet, so no credential verifies"
			if j.fetched() {
				msg = "credential: JWKS refresh failed; answering from cached keys"
			}
			slog.Warn(msg, "url", j.url, "err", err)
		} else {
			interval = jwksRefreshInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Key returns the public key for kid. An unknown kid triggers one refetch, at
// most once per jwksUnknownKIDInterval, so a newly rotated key works at once.
func (j *JWKS) Key(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	j.mu.Lock()
	pub, err := j.lookupLocked(kid)
	if !errors.Is(err, errUnknownKey) || j.now().Sub(j.lastUnknownFetch) < jwksUnknownKIDInterval {
		j.mu.Unlock()
		return pub, err
	}
	j.lastUnknownFetch = j.now()
	j.mu.Unlock()

	if err := j.refresh(ctx); err != nil {
		slog.Warn("credential: JWKS refetch for an unknown kid failed", "url", j.url, "err", err)
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	return j.lookupLocked(kid)
}

// fetched reports whether a fetch has ever succeeded.
func (j *JWKS) fetched() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.keys != nil
}

func (j *JWKS) lookupLocked(kid string) (ed25519.PublicKey, error) {
	if j.keys == nil {
		return nil, errNoKeys
	}
	if j.now().Sub(j.lastSuccess) > jwksMaxStaleness {
		return nil, errKeysStale
	}
	pub, ok := j.keys[kid]
	if !ok {
		return nil, errUnknownKey
	}
	return pub, nil
}

// jwk is one entry of the control plane's key set.
type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid"`
}

func (j *JWKS) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.url, nil)
	if err != nil {
		return fmt.Errorf("jwks: build request: %w", err)
	}
	resp, err := j.client.Do(req)
	if err != nil {
		return fmt.Errorf("jwks: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, jwksMaxBody))
	if err != nil {
		return fmt.Errorf("jwks: read: %w", err)
	}

	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return fmt.Errorf("jwks: decode: %w", err)
	}
	keys := make(map[string]ed25519.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" || k.Kid == "" {
			continue // not a key a relay can use
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			continue
		}
		keys[k.Kid] = ed25519.PublicKey(x)
	}
	if len(keys) == 0 {
		return errNoUsableJWKSKey
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	j.keys = keys
	j.lastSuccess = j.now()
	return nil
}
