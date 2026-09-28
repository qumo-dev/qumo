package relay

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
	"strings"
	"sync"
	"time"
)

// Local credential verification (qumo-deploy ADR 0034): the relay checks a
// credential itself against the control plane's public keys instead of asking
// the control plane per announcement. The control plane signs; relays verify.
//
// This path serves a relay a customer runs for a qumo-deploy dev project. Such
// a relay cannot read the operator-authenticated revocation feed, so it
// verifies with the JWKS alone and a revoked credential stops working at its
// exp (ADR 0034, "Revocation on a customer's relay"). Managed relays keep
// introspection until they consume the feed (qumo-dev/qumo#419).

const (
	// credentialLeeway absorbs clock skew between the control plane and the
	// relay when checking exp, nbf and iat.
	credentialLeeway = 60 * time.Second

	// jwksRefreshInterval matches the endpoint's Cache-Control max-age.
	jwksRefreshInterval = 5 * time.Minute
	// jwksRetryInterval paces fetches until the first one succeeds; no
	// credential is admitted before then.
	jwksRetryInterval = 10 * time.Second
	// jwksUnknownKIDInterval bounds refetches triggered by an unknown kid, so
	// a stream of forged kids cannot turn into a stream of fetches.
	jwksUnknownKIDInterval = 30 * time.Second
	// jwksMaxStaleness is how long the relay keeps admitting on cached keys
	// after refreshes start failing (fail-static). A public key never expires,
	// so failing closed at once would put the control plane back in the
	// admission path; the cap bounds exposure to a removed, compromised key.
	jwksMaxStaleness = 6 * time.Hour

	jwksPath = "/v1/credentials/jwks"
)

var (
	errMalformedCredential = errors.New("credential: malformed")
	errUnsupportedAlg      = errors.New("credential: algorithm is not EdDSA")
	errUnknownKey          = errors.New("credential: signing key not in the JWKS")
	errBadSignature        = errors.New("credential: signature does not verify")
	errExpired             = errors.New("credential: expired")
	errNotYetValid         = errors.New("credential: not yet valid")
	errWrongIssuer         = errors.New("credential: issuer does not match")
	errWrongAudience       = errors.New("credential: audience does not match")
	errPathNotCovered      = errors.New("credential: path not covered by the credential")
	errNoKeys              = errors.New("credential: JWKS not fetched yet")
	errKeysStale           = errors.New("credential: JWKS too stale to admit")
)

// signingKeys resolves a JWKS key id to its Ed25519 public key.
type signingKeys interface {
	key(ctx context.Context, kid string) (ed25519.PublicKey, error)
}

// localVerifier checks relay credentials locally: signature by kid, time
// claims with leeway, issuer, audience and the announced path.
type localVerifier struct {
	keys     signingKeys
	issuer   string
	audience string
	now      func() time.Time
}

// credentialHeader is the JWS protected header.
type credentialHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

// credentialPathAuth mirrors qumo-deploy's path_auth claim. A nil Pub means no
// publish grant; an empty one means everything under Root.
type credentialPathAuth struct {
	Root string  `json:"root"`
	Pub  *string `json:"pub"`
	Sub  *string `json:"sub"`
}

// credentialClaims holds the claims the relay checks. Numeric dates are
// float64 because JSON numbers may carry a fraction.
type credentialClaims struct {
	ID        string              `json:"jti"`
	Issuer    string              `json:"iss"`
	Audience  audienceClaim       `json:"aud"`
	ExpiresAt *float64            `json:"exp"`
	NotBefore *float64            `json:"nbf"`
	IssuedAt  *float64            `json:"iat"`
	PathAuth  *credentialPathAuth `json:"path_auth"`
}

// audienceClaim accepts aud as a single string or an array of strings, the
// two forms RFC 7519 allows.
type audienceClaim []string

func (a *audienceClaim) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audienceClaim{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("aud: %w", err)
	}
	*a = many
	return nil
}

func (a audienceClaim) contains(want string) bool {
	for _, v := range a {
		if v == want {
			return true
		}
	}
	return false
}

// verify checks token for announcing broadcastPath and returns its jti.
func (v *localVerifier) verify(ctx context.Context, token, broadcastPath string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errMalformedCredential
	}

	var header credentialHeader
	if err := decodeSegment(parts[0], &header); err != nil {
		return "", fmt.Errorf("%w: header: %w", errMalformedCredential, err)
	}
	if header.Alg != "EdDSA" {
		return "", errUnsupportedAlg
	}
	if header.Kid == "" {
		return "", fmt.Errorf("%w: no kid", errMalformedCredential)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return "", fmt.Errorf("%w: signature encoding", errMalformedCredential)
	}
	pub, err := v.keys.key(ctx, header.Kid)
	if err != nil {
		return "", err
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return "", errBadSignature
	}

	var claims credentialClaims
	if err := decodeSegment(parts[1], &claims); err != nil {
		return "", fmt.Errorf("%w: claims: %w", errMalformedCredential, err)
	}
	if err := v.checkClaims(&claims); err != nil {
		return "", err
	}
	if !claims.PathAuth.coversPublish(broadcastPath) {
		return "", errPathNotCovered
	}
	return claims.ID, nil
}

func (v *localVerifier) checkClaims(c *credentialClaims) error {
	if c.ID == "" || c.ExpiresAt == nil {
		return fmt.Errorf("%w: jti and exp are required", errMalformedCredential)
	}
	now := v.now()
	if now.After(numericDate(*c.ExpiresAt).Add(credentialLeeway)) {
		return errExpired
	}
	if c.NotBefore != nil && now.Add(credentialLeeway).Before(numericDate(*c.NotBefore)) {
		return errNotYetValid
	}
	if c.IssuedAt != nil && now.Add(credentialLeeway).Before(numericDate(*c.IssuedAt)) {
		return errNotYetValid
	}
	if strings.TrimRight(c.Issuer, "/") != v.issuer {
		return errWrongIssuer
	}
	if !c.Audience.contains(v.audience) {
		return errWrongAudience
	}
	return nil
}

// coversPublish reports whether the grant may announce broadcastPath: the
// path must equal root+pub or lie beneath it at a segment boundary. It
// matches qumo-deploy's CredentialClaims.coversPublish exactly, so the relay
// and the control plane never disagree on a path.
func (p *credentialPathAuth) coversPublish(broadcastPath string) bool {
	if p == nil || p.Pub == nil {
		return false
	}
	path := normalizeBroadcastPath(broadcastPath)
	if path == "" {
		return false
	}
	scope := normalizeBroadcastPath(p.Root)
	if pub := normalizeBroadcastPath(*p.Pub); pub != "" {
		scope += "/" + pub
	}
	if scope == "" {
		return false
	}
	return path == scope || strings.HasPrefix(path, scope+"/")
}

// normalizeBroadcastPath trims surrounding slashes and collapses repeated
// ones, so "/tenant/project//live/" equals "tenant/project/live" (MoQ clients
// announce with a leading slash; path_auth has none).
func normalizeBroadcastPath(p string) string {
	parts := strings.Split(p, "/")
	kept := parts[:0]
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "/")
}

func decodeSegment(seg string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func numericDate(secs float64) time.Time {
	whole := int64(secs)
	return time.Unix(whole, int64((secs-float64(whole))*float64(time.Second)))
}

// jwksCache holds the control plane's public keys by kid. It fetches at
// start, refreshes on a timer, refetches on an unknown kid (rate-limited), and
// keeps the last good set when a refresh fails until jwksMaxStaleness.
type jwksCache struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu               sync.Mutex
	keys             map[string]ed25519.PublicKey
	lastSuccess      time.Time
	lastUnknownFetch time.Time
}

var _ signingKeys = (*jwksCache)(nil)

func newJWKSCache(baseURL string, client *http.Client) *jwksCache {
	return &jwksCache{url: baseURL + jwksPath, client: client, now: time.Now}
}

// run fetches until the first success, then refreshes every
// jwksRefreshInterval, until ctx is done.
func (c *jwksCache) run(ctx context.Context) {
	interval := jwksRetryInterval
	for {
		if err := c.refresh(ctx); err != nil {
			slog.Warn("relay: JWKS refresh failed; admitting on cached keys", "err", err)
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

// key returns the public key for kid. An unknown kid triggers one refetch, at
// most once per jwksUnknownKIDInterval, so a newly rotated key works at once.
func (c *jwksCache) key(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	c.mu.Lock()
	pub, err := c.lookupLocked(kid)
	if !errors.Is(err, errUnknownKey) || c.now().Sub(c.lastUnknownFetch) < jwksUnknownKIDInterval {
		c.mu.Unlock()
		return pub, err
	}
	c.lastUnknownFetch = c.now()
	c.mu.Unlock()

	if err := c.refresh(ctx); err != nil {
		slog.Warn("relay: JWKS refetch for an unknown kid failed", "err", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lookupLocked(kid)
}

func (c *jwksCache) lookupLocked(kid string) (ed25519.PublicKey, error) {
	if c.keys == nil {
		return nil, errNoKeys
	}
	if c.now().Sub(c.lastSuccess) > jwksMaxStaleness {
		return nil, errKeysStale
	}
	pub, ok := c.keys[kid]
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

func (c *jwksCache) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return fmt.Errorf("jwks: build request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("jwks: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, credentialMaxBody))
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
			continue // not a key this relay can use
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			continue
		}
		keys[k.Kid] = ed25519.PublicKey(x)
	}
	if len(keys) == 0 {
		return errors.New("jwks: no usable Ed25519 keys")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys = keys
	c.lastSuccess = c.now()
	return nil
}
