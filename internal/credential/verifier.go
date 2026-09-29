// Package credential verifies qumo-deploy relay credentials locally.
//
// A relay credential is an EdDSA (Ed25519) JWT the control plane signs. The
// control plane publishes its public keys as a JWKS, so a relay can check a
// credential itself instead of asking the control plane per announcement: the
// control plane signs; relays verify (qumo-deploy ADR 0034).
//
// Verifier checks one credential. JWKS keeps the key set it verifies against:
// fetched at start, refreshed on a timer, and kept when a refresh fails.
//
// The package knows nothing about relays, sessions or metering: it takes a
// token and a broadcast path, and answers whether the token may publish there.
package credential

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ManagedAudience is the credential audience of the managed relay network.
// qumo-deploy issues it to prod-project keys; dev-project keys get a
// different one, which only a customer's own relay accepts.
const ManagedAudience = "qumo-relay"

// leeway absorbs clock skew between the control plane and the relay when
// checking exp, nbf and iat.
const leeway = 60 * time.Second

var (
	errMalformed       = errors.New("credential: malformed")
	errUnsupportedAlg  = errors.New("credential: algorithm is not EdDSA")
	errUnknownKey      = errors.New("credential: signing key not in the JWKS")
	errBadSignature    = errors.New("credential: signature does not verify")
	errExpired         = errors.New("credential: expired")
	errNotYetValid     = errors.New("credential: not yet valid")
	errWrongIssuer     = errors.New("credential: issuer does not match")
	errWrongAudience   = errors.New("credential: audience does not match")
	errPathNotCovered  = errors.New("credential: path not covered by the credential")
	errNoKeys          = errors.New("credential: JWKS not fetched yet")
	errKeysStale       = errors.New("credential: JWKS too stale to admit")
	errNoUsableJWKSKey = errors.New("jwks: no usable Ed25519 keys")
)

// Keys resolves a JWKS key id to its Ed25519 public key. JWKS is the
// implementation a relay uses; tests supply a fixed set.
type Keys interface {
	Key(ctx context.Context, kid string) (ed25519.PublicKey, error)
}

// Verifier checks relay credentials locally: signature by kid, time claims
// with leeway, issuer, audience and the announced path.
type Verifier struct {
	keys     Keys
	issuer   string
	audience string
	now      func() time.Time
}

// NewVerifier returns a Verifier that accepts credentials signed by a key in
// keys, issued by issuer (the control plane's public URL) for audience.
func NewVerifier(keys Keys, issuer, audience string) *Verifier {
	return &Verifier{
		keys:     keys,
		issuer:   strings.TrimRight(issuer, "/"),
		audience: audience,
		now:      time.Now,
	}
}

// header is the JWS protected header.
type header struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

// pathAuth mirrors qumo-deploy's path_auth claim. A nil Pub means no publish
// grant; an empty one means everything under Root.
type pathAuth struct {
	Root string  `json:"root"`
	Pub  *string `json:"pub"`
	Sub  *string `json:"sub"`
}

// claims holds the claims a relay checks. Numeric dates are float64 because
// JSON numbers may carry a fraction.
type claims struct {
	ID        string    `json:"jti"`
	Issuer    string    `json:"iss"`
	Audience  audiences `json:"aud"`
	ExpiresAt *float64  `json:"exp"`
	NotBefore *float64  `json:"nbf"`
	IssuedAt  *float64  `json:"iat"`
	PathAuth  *pathAuth `json:"path_auth"`
}

// audiences accepts aud as a single string or an array of strings, the two
// forms RFC 7519 allows.
type audiences []string

func (a *audiences) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audiences{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("aud: %w", err)
	}
	*a = many
	return nil
}

func (a audiences) contains(want string) bool {
	for _, v := range a {
		if v == want {
			return true
		}
	}
	return false
}

// VerifyPublish checks that token may announce broadcastPath and returns the
// credential's id (jti).
func (v *Verifier) VerifyPublish(ctx context.Context, token, broadcastPath string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errMalformed
	}

	var h header
	if err := decodeSegment(parts[0], &h); err != nil {
		return "", fmt.Errorf("%w: header: %w", errMalformed, err)
	}
	if h.Alg != "EdDSA" {
		return "", errUnsupportedAlg
	}
	if h.Kid == "" {
		return "", fmt.Errorf("%w: no kid", errMalformed)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return "", fmt.Errorf("%w: signature encoding", errMalformed)
	}
	pub, err := v.keys.Key(ctx, h.Kid)
	if err != nil {
		return "", err
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return "", errBadSignature
	}

	var c claims
	if err := decodeSegment(parts[1], &c); err != nil {
		return "", fmt.Errorf("%w: claims: %w", errMalformed, err)
	}
	if err := v.checkClaims(&c); err != nil {
		return "", err
	}
	if !c.PathAuth.coversPublish(broadcastPath) {
		return "", errPathNotCovered
	}
	return c.ID, nil
}

func (v *Verifier) checkClaims(c *claims) error {
	if c.ID == "" || c.ExpiresAt == nil {
		return fmt.Errorf("%w: jti and exp are required", errMalformed)
	}
	now := v.now()
	if now.After(numericDate(*c.ExpiresAt).Add(leeway)) {
		return errExpired
	}
	if c.NotBefore != nil && now.Add(leeway).Before(numericDate(*c.NotBefore)) {
		return errNotYetValid
	}
	if c.IssuedAt != nil && now.Add(leeway).Before(numericDate(*c.IssuedAt)) {
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
func (p *pathAuth) coversPublish(broadcastPath string) bool {
	if p == nil || p.Pub == nil {
		return false
	}
	path := normalizePath(broadcastPath)
	if path == "" {
		return false
	}
	scope := normalizePath(p.Root)
	if pub := normalizePath(*p.Pub); pub != "" {
		scope += "/" + pub
	}
	if scope == "" {
		return false
	}
	return path == scope || strings.HasPrefix(path, scope+"/")
}

// normalizePath trims surrounding slashes and collapses repeated ones, so
// "/tenant/project//live/" equals "tenant/project/live" (MoQ clients announce
// with a leading slash; path_auth has none).
func normalizePath(p string) string {
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
