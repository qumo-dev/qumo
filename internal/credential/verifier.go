// Package credential verifies relay credentials locally.
//
// A relay credential is an EdDSA (Ed25519) JWT the app signs with a key it
// registered with qumo; the relay checks it against the public keys it trusts,
// with no call to the control plane (qumo-deploy ADR 0035).
//
// Verifier checks one credential. StaticKeys is the key set a relay loads from
// its own configuration.
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

const (
	// leeway absorbs clock skew between the app and the relay when checking
	// exp, nbf and iat.
	leeway = 60 * time.Second
	// maxLifetime caps exp - iat, so a leaked credential is bounded by it.
	maxLifetime = time.Hour
)

var (
	errMalformed       = errors.New("credential: malformed")
	errUnsupportedAlg  = errors.New("credential: algorithm is not EdDSA")
	errUnknownKey      = errors.New("credential: signing key not trusted")
	errBadSignature    = errors.New("credential: signature does not verify")
	errExpired         = errors.New("credential: expired")
	errNotYetValid     = errors.New("credential: not yet valid")
	errLifetimeTooLong = errors.New("credential: lifetime exceeds the cap")
	errPathNotCovered  = errors.New("credential: path not covered by the credential")
	errOutsidePrefix   = errors.New("credential: grant outside the signing key's prefix")
)

// Keys resolves a key id to the trusted key.
type Keys interface {
	Key(ctx context.Context, kid string) (Key, error)
}

// Verifier checks relay credentials locally: signature by kid, time claims
// with leeway and the lifetime cap, and the announced path.
type Verifier struct {
	keys Keys
	now  func() time.Time
}

// NewVerifier returns a Verifier that accepts credentials signed by a key in
// keys.
func NewVerifier(keys Keys) *Verifier {
	return &Verifier{keys: keys, now: time.Now}
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

// claims holds the claims a relay checks; any other claim is ignored. Numeric
// dates are float64 because JSON numbers may carry a fraction.
type claims struct {
	ExpiresAt *float64  `json:"exp"`
	NotBefore *float64  `json:"nbf"`
	IssuedAt  *float64  `json:"iat"`
	PathAuth  *pathAuth `json:"path_auth"`
}

// Credential is a verified credential: the key that signed it, when it
// expires, and what it grants.
type Credential struct {
	Key       Key
	ExpiresAt time.Time
	grants    pathAuth
}

// CoversPublish reports whether the credential may announce broadcastPath.
func (c Credential) CoversPublish(broadcastPath string) bool {
	return c.grants.coversPublish(broadcastPath)
}

// CoversSubscribe reports whether the credential may subscribe to
// broadcastPath.
func (c Credential) CoversSubscribe(broadcastPath string) bool {
	return c.grants.coversSubscribe(broadcastPath)
}

// Within reports whether c grants nothing prev does not: every publish grant
// of c lies at or beneath prev's publish grant, and likewise for subscribe.
// A refreshed credential must satisfy it, so a refresh can narrow what a
// session may do but never widen it.
func (c Credential) Within(prev Credential) bool {
	for _, g := range []struct{ next, prev *string }{
		{c.grants.Pub, prev.grants.Pub},
		{c.grants.Sub, prev.grants.Sub},
	} {
		if g.next == nil {
			continue
		}
		if g.prev == nil || !beneath(c.grants.scope(*g.next), prev.grants.scope(*g.prev)) {
			return false
		}
	}
	return true
}

// Verify runs the checks every credential must pass: a trusted kid, the EdDSA
// signature, the time claims and lifetime cap, and prefix confinement.
func (v *Verifier) Verify(ctx context.Context, token string) (Credential, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Credential{}, errMalformed
	}

	var h header
	if err := decodeSegment(parts[0], &h); err != nil {
		return Credential{}, fmt.Errorf("%w: header: %w", errMalformed, err)
	}
	if h.Alg != "EdDSA" {
		return Credential{}, errUnsupportedAlg
	}
	if h.Kid == "" {
		return Credential{}, fmt.Errorf("%w: no kid", errMalformed)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return Credential{}, fmt.Errorf("%w: signature encoding", errMalformed)
	}
	key, err := v.keys.Key(ctx, h.Kid)
	if err != nil {
		return Credential{}, err
	}
	if !ed25519.Verify(key.Public, []byte(parts[0]+"."+parts[1]), sig) {
		return Credential{}, errBadSignature
	}

	var c claims
	if err := decodeSegment(parts[1], &c); err != nil {
		return Credential{}, fmt.Errorf("%w: claims: %w", errMalformed, err)
	}
	if err := v.checkClaims(&c); err != nil {
		return Credential{}, err
	}
	if err := c.PathAuth.within(key.Prefix); err != nil {
		return Credential{}, err
	}
	cred := Credential{Key: key, ExpiresAt: numericDate(*c.ExpiresAt)}
	if c.PathAuth != nil {
		cred.grants = *c.PathAuth
	}
	return cred, nil
}

// VerifyPublish checks that token may announce broadcastPath.
func (v *Verifier) VerifyPublish(ctx context.Context, token, broadcastPath string) (Credential, error) {
	cred, err := v.Verify(ctx, token)
	if err != nil {
		return Credential{}, err
	}
	if !cred.CoversPublish(broadcastPath) {
		return Credential{}, errPathNotCovered
	}
	return cred, nil
}

func (v *Verifier) checkClaims(c *claims) error {
	if c.ExpiresAt == nil || c.IssuedAt == nil {
		return fmt.Errorf("%w: exp and iat are required", errMalformed)
	}
	exp, iat := numericDate(*c.ExpiresAt), numericDate(*c.IssuedAt)
	if exp.Sub(iat) > maxLifetime {
		return errLifetimeTooLong
	}
	now := v.now()
	if now.After(exp.Add(leeway)) {
		return errExpired
	}
	if c.NotBefore != nil && now.Add(leeway).Before(numericDate(*c.NotBefore)) {
		return errNotYetValid
	}
	if now.Add(leeway).Before(iat) {
		return errNotYetValid
	}
	return nil
}

// within checks the credential's grants against its key's prefix (ADR 0035,
// check 4): every granted path, root+pub and root+sub, must equal the prefix
// or lie beneath it at a segment boundary. This is what stops a registered key
// from signing for another tenant's paths. An empty prefix leaves the key
// unconstrained (a statically configured key). A grant with a "." or ".."
// segment is refused outright rather than interpreted.
func (p *pathAuth) within(prefix string) error {
	if p == nil {
		return nil
	}
	for _, grant := range []*string{p.Pub, p.Sub} {
		if grant == nil {
			continue
		}
		if hasDotSegment(p.Root) || hasDotSegment(*grant) {
			return fmt.Errorf("%w: dot segment in path_auth", errMalformed)
		}
		if prefix == "" {
			continue
		}
		if !beneath(p.scope(*grant), normalizePath(prefix)) {
			return errOutsidePrefix
		}
	}
	return nil
}

// coversPublish reports whether the grant may announce broadcastPath: the
// path must equal root+pub or lie beneath it at a segment boundary.
func (p *pathAuth) coversPublish(broadcastPath string) bool {
	if p == nil || p.Pub == nil {
		return false
	}
	return beneath(normalizePath(broadcastPath), p.scope(*p.Pub))
}

// coversSubscribe reports whether the grant may subscribe to broadcastPath:
// the path must equal root+sub or lie beneath it at a segment boundary.
func (p *pathAuth) coversSubscribe(broadcastPath string) bool {
	if p == nil || p.Sub == nil {
		return false
	}
	return beneath(normalizePath(broadcastPath), p.scope(*p.Sub))
}

// scope is the path a grant covers: root joined with the grant, normalized.
func (p *pathAuth) scope(grant string) string {
	return normalizePath(p.Root + "/" + grant)
}

// beneath reports whether path equals scope or lies beneath it at a segment
// boundary. An empty path or scope never matches.
func beneath(path, scope string) bool {
	if path == "" || scope == "" {
		return false
	}
	return path == scope || strings.HasPrefix(path, scope+"/")
}

// hasDotSegment reports whether p has a "." or ".." segment.
func hasDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
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
