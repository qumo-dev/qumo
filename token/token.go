// Package token signs and verifies qumo capability tokens: EdDSA JWTs an app
// signs with its own Ed25519 key to say what a client may publish and
// subscribe to on a qumo relay.
//
// An app's backend signs one per client with Sign and hands it to the client,
// which connects with it in the relay URL (?jwt=…). The relay checks it
// with Verify. The app decides who may do what; this package is the machinery: the key format, the signature, the
// time claims and confining every path to the key's prefix.
package token

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// Leeway absorbs clock skew between the app that signed a token and the
	// verifier when checking exp, nbf and iat.
	Leeway = 60 * time.Second
	// MaxLifetime caps a token's lifetime (exp - iat), so a leaked token is
	// bounded by it.
	MaxLifetime = time.Hour
)

var (
	// ErrInvalid is a token that can't be accepted: malformed, signed by an
	// unknown key or not by the key it names, with claims outside the
	// allowed set, or expired or not yet valid.
	ErrInvalid = errors.New("token: invalid")
	// ErrForbidden is a valid token that grants nothing usable, or grants a
	// path outside its key's prefix.
	ErrForbidden = errors.New("token: forbidden")
)

// allowedClaims is the whole claim set. Any other claim refuses the token: an
// unknown claim might be meant to narrow the grant, and a misspelled
// path_auth member must not widen it.
var allowedClaims = map[string]bool{"path_auth": true, "iat": true, "nbf": true, "exp": true, "jti": true}

// Grant is what a token allows its bearer: the path it may publish at or
// beneath, and the path it may subscribe at or beneath. An empty one grants
// nothing for that role. A token must grant at least one.
type Grant struct {
	Publish   string
	Subscribe string
}

// Claims are a verified token's: its grant, normalized and confined to its
// key's prefix, when it expires, its jti if any, and the key that signed it.
type Claims struct {
	Grant
	ExpiresAt time.Time
	ID        string
	Key       Key
}

// pathAuth is the capability claim, per the @moq/token convention: root plus
// pub or sub. A nil Pub or Sub grants nothing for that role.
type pathAuth struct {
	Root string  `json:"root"`
	Pub  *string `json:"pub,omitempty"`
	Sub  *string `json:"sub,omitempty"`
}

// claims is a token's claim set as encoded. Numeric dates are float64 on
// decode because JSON numbers may carry a fraction.
type claims struct {
	PathAuth  *pathAuth `json:"path_auth"`
	IssuedAt  *float64  `json:"iat"`
	NotBefore *float64  `json:"nbf"`
	ExpiresAt *float64  `json:"exp"`
	ID        string    `json:"jti,omitempty"`
}

// Sign returns a token signed by key that grants g for ttl from now, at most
// MaxLifetime. It carries a random jti. A grant outside key's prefix is
// refused here, since a verifier would refuse the token.
func Sign(key SigningKey, g Grant, ttl time.Duration) (string, error) {
	return signAt(key, g, ttl, time.Now())
}

func signAt(key SigningKey, g Grant, ttl time.Duration, now time.Time) (string, error) {
	if ttl <= 0 || ttl > MaxLifetime {
		return "", fmt.Errorf("token: ttl %s must be positive and at most %s", ttl, MaxLifetime)
	}
	if g.Publish == "" && g.Subscribe == "" {
		return "", errors.New("token: the grant names neither a publish nor a subscribe path")
	}
	pa := &pathAuth{}
	for _, role := range []struct {
		path string
		into **string
	}{{g.Publish, &pa.Pub}, {g.Subscribe, &pa.Sub}} {
		if role.path == "" {
			continue
		}
		norm, err := normalizePath(role.path)
		if err != nil || norm == "" {
			return "", fmt.Errorf("token: grant path %q: want a path with no \".\", \"..\" or \"*\" segments", role.path)
		}
		if !within(norm, key.Prefix) {
			return "", fmt.Errorf("token: grant path %q lies outside the signing key's prefix %q", norm, key.Prefix)
		}
		*role.into = &norm
	}
	iat := float64(now.Unix())
	exp := float64(now.Add(ttl).Unix())
	c := claims{PathAuth: pa, IssuedAt: &iat, NotBefore: &iat, ExpiresAt: &exp, ID: rand.Text()}

	header, err := json.Marshal(map[string]string{"alg": "EdDSA", "kid": key.ID, "typ": "JWT"})
	if err != nil {
		return "", fmt.Errorf("token: encode header: %w", err)
	}
	body, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("token: encode claims: %w", err)
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key.Private, []byte(input))), nil
}

// Verify checks token against the trusted keys at now, in order: the header
// carries no crit, the alg is EdDSA and the kid is trusted; the signature
// verifies; the claims are
// exactly the allowed set (path_auth, iat, nbf, exp, and an optional jti)
// with no duplicate members; exp, nbf and iat hold within Leeway and the
// lifetime is at most MaxLifetime; and every path the token grants lies
// within its key's prefix. An error wraps ErrInvalid or ErrForbidden.
func Verify(token string, keys map[string]Key, now time.Time) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, invalid("not a JWS compact serialization")
	}
	var header struct {
		Alg  string         `json:"alg"`
		Kid  string         `json:"kid"`
		Typ  string         `json:"typ"`
		Crit jsontext.Value `json:"crit"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return Claims{}, invalid("header: %v", err)
	}
	// RFC 7515 4.1.11: crit lists headers the verifier must understand. This
	// one understands none beyond alg, kid and typ, so any crit is refused.
	if len(header.Crit) > 0 {
		return Claims{}, invalid("header: crit is not supported")
	}
	if header.Alg != "EdDSA" {
		return Claims{}, invalid("alg %q is not EdDSA", header.Alg)
	}
	key, ok := keys[header.Kid]
	if !ok {
		return Claims{}, invalid("signing key %q is not trusted", header.Kid)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(key.Public, []byte(parts[0]+"."+parts[1]), sig) {
		return Claims{}, invalid("signature does not verify")
	}

	var members map[string]jsontext.Value
	if err := decodeSegment(parts[1], &members); err != nil {
		return Claims{}, invalid("claims: %v", err)
	}
	for name := range members {
		if !allowedClaims[name] {
			return Claims{}, invalid("claim %q is not allowed", name)
		}
	}
	var c claims
	if err := decodeSegment(parts[1], &c); err != nil {
		return Claims{}, invalid("claims: %v", err)
	}
	exp, err := c.checkTime(now)
	if err != nil {
		return Claims{}, err
	}
	if c.PathAuth == nil {
		return Claims{}, invalid("path_auth is required")
	}
	g, err := grantOf(*c.PathAuth, key.Prefix)
	if err != nil {
		return Claims{}, err
	}
	return Claims{Grant: g, ExpiresAt: exp, ID: c.ID, Key: key}, nil
}

// grantOf resolves path_auth into the paths it grants, each of which must lie
// within prefix at a "/" boundary: the rule that stops a key from signing for
// another tenant's paths.
func grantOf(pa pathAuth, prefix string) (Grant, error) {
	var g Grant
	for _, role := range []struct {
		name   string
		suffix *string
		into   *string
	}{{"pub", pa.Pub, &g.Publish}, {"sub", pa.Sub, &g.Subscribe}} {
		if role.suffix == nil {
			continue
		}
		scope, err := scopeOf(pa.Root, *role.suffix)
		if err != nil {
			return Grant{}, invalid("path_auth %s: %v", role.name, err)
		}
		if !within(scope, prefix) {
			return Grant{}, forbidden("path_auth %s %q lies outside the signing key's prefix %q", role.name, scope, prefix)
		}
		*role.into = scope
	}
	if g.Publish == "" && g.Subscribe == "" {
		return Grant{}, forbidden("path_auth grants neither pub nor sub")
	}
	return g, nil
}

// checkTime enforces exp, nbf and iat (all required) with Leeway, and the
// lifetime cap, and returns exp.
func (c claims) checkTime(now time.Time) (time.Time, error) {
	if c.ExpiresAt == nil || c.NotBefore == nil || c.IssuedAt == nil {
		return time.Time{}, invalid("exp, nbf and iat are required")
	}
	exp, nbf, iat := numericDate(*c.ExpiresAt), numericDate(*c.NotBefore), numericDate(*c.IssuedAt)
	if lifetime := exp.Sub(iat); lifetime <= 0 || lifetime > MaxLifetime {
		return time.Time{}, invalid("lifetime (exp - iat) must be positive and at most %s", MaxLifetime)
	}
	// Judged on whole seconds, the precision of the expiry a relay enforces.
	switch {
	case !now.Before(time.Unix(exp.Add(Leeway).Unix(), 0)):
		return time.Time{}, invalid("expired")
	case now.Add(Leeway).Before(nbf), now.Add(Leeway).Before(iat):
		return time.Time{}, invalid("not valid yet")
	}
	return exp, nil
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func forbidden(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrForbidden, fmt.Sprintf(format, args...))
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
