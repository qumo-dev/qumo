package authserver

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	// leeway absorbs clock skew between the app that signed a token and the
	// auth server when checking exp, nbf and iat.
	leeway = 60 * time.Second
	// maxLifetime caps exp - iat, so a leaked token is bounded by it.
	maxLifetime = time.Hour
)

// allowedClaims is the whole claim set (ADR 0035, Decision 2). Any other
// claim refuses the token: an unknown claim might be meant to narrow the
// grant, and a misspelled path_auth member must not widen it.
var allowedClaims = map[string]bool{"path_auth": true, "iat": true, "nbf": true, "exp": true, "jti": true}

// refusal is why a token or request was turned down, with the HTTP status the
// relay receives: 401 for a token that can't be accepted, 403 for a valid
// token that grants nothing usable.
type refusal struct {
	status int
	reason string
}

func (r refusal) Error() string { return r.reason }

func refuse(format string, args ...any) refusal {
	return refusal{status: http.StatusUnauthorized, reason: fmt.Sprintf(format, args...)}
}

func forbid(format string, args ...any) refusal {
	return refusal{status: http.StatusForbidden, reason: fmt.Sprintf(format, args...)}
}

// pathAuth is the capability: what the bearer may publish and subscribe to,
// per the @moq/token convention. A nil Pub or Sub grants nothing for that
// role; an empty one grants everything under Root.
type pathAuth struct {
	Root string  `json:"root"`
	Pub  *string `json:"pub"`
	Sub  *string `json:"sub"`
}

// claims holds a verified token's claims. Numeric dates are float64 because
// JSON numbers may carry a fraction.
type claims struct {
	PathAuth  *pathAuth `json:"path_auth"`
	IssuedAt  *float64  `json:"iat"`
	NotBefore *float64  `json:"nbf"`
	ExpiresAt *float64  `json:"exp"`
	ID        string    `json:"jti"`
}

// verify checks token against keys at now and returns its claims and the key
// that signed it: the kid is known, the EdDSA signature holds, the claims are
// exactly the allowed set, and the time claims hold within the lifetime cap.
// Path checks are grantFor's.
func verify(token string, keys map[string]Key, now time.Time) (claims, Key, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims{}, Key{}, refuse("token: not a JWS compact serialization")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return claims{}, Key{}, refuse("token: header: %v", err)
	}
	if header.Alg != "EdDSA" {
		return claims{}, Key{}, refuse("token: alg %q is not EdDSA", header.Alg)
	}
	key, ok := keys[header.Kid]
	if !ok {
		return claims{}, Key{}, refuse("token: signing key %q is not trusted", header.Kid)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(key.Public, []byte(parts[0]+"."+parts[1]), sig) {
		return claims{}, Key{}, refuse("token: signature does not verify")
	}

	var members map[string]jsontext.Value
	if err := decodeSegment(parts[1], &members); err != nil {
		return claims{}, Key{}, refuse("token: claims: %v", err)
	}
	for name := range members {
		if !allowedClaims[name] {
			return claims{}, Key{}, refuse("token: claim %q is not allowed", name)
		}
	}
	var c claims
	if err := decodeSegment(parts[1], &c); err != nil {
		return claims{}, Key{}, refuse("token: claims: %v", err)
	}
	if err := c.checkTime(now); err != nil {
		return claims{}, Key{}, err
	}
	if c.PathAuth == nil {
		return claims{}, Key{}, refuse("token: path_auth is required")
	}
	return c, key, nil
}

// checkTime enforces exp, nbf and iat (all required) with leeway, and the
// lifetime cap.
func (c claims) checkTime(now time.Time) error {
	if c.ExpiresAt == nil || c.NotBefore == nil || c.IssuedAt == nil {
		return refuse("token: exp, nbf and iat are required")
	}
	exp, nbf, iat := numericDate(*c.ExpiresAt), numericDate(*c.NotBefore), numericDate(*c.IssuedAt)
	if lifetime := exp.Sub(iat); lifetime <= 0 || lifetime > maxLifetime {
		return refuse("token: lifetime (exp - iat) must be positive and at most %s", maxLifetime)
	}
	// The grant's expires is whole seconds (grantFor), so expiry is judged
	// on the same value the relay will enforce.
	switch {
	case !now.Before(time.Unix(exp.Add(leeway).Unix(), 0)):
		return refuse("token: expired")
	case now.Add(leeway).Before(nbf), now.Add(leeway).Before(iat):
		return refuse("token: not valid yet")
	}
	return nil
}

func numericDate(v float64) time.Time {
	sec := int64(v)
	return time.Unix(sec, int64((v-float64(sec))*float64(time.Second)))
}

// decodeSegment decodes one base64url JWS segment as JSON into v. Duplicate
// member names are an error, so a claim can't be smuggled in twice.
func decodeSegment(seg string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return fmt.Errorf("not base64url: %w", err)
	}
	return json.UnmarshalRead(bytes.NewReader(raw), v)
}
