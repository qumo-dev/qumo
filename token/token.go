// Package token signs and verifies qumo capability tokens: EdDSA JWTs an app
// signs with its own Ed25519 key to say what a client may do on a qumo relay
// or funnel.
//
// A token grants what a MoQ grant (moqt.Grant) does: a root, the directory of
// broadcast paths it applies to, and for each action the path patterns
// beneath the root it permits (publish and subscribe at a relay; create,
// post, subscribe to read history, and redact at a funnel), on every track of
// each broadcast they match. It may name its bearer in sub. A relay hands the
// root and the publish and subscribe patterns to its MoQ session unchanged.
//
// Sign writes the root, publish, subscribe, create, post and redact claims.
// Verify also accepts the path_auth claim of other MoQ implementations (the
// @moq/token convention) and reads it as a grant: its root as the root, pub
// as publish, and sub as subscribe, each on its path and every path beneath
// it. A token carries one of the two forms, never both.
//
// An app's backend signs one per client with Sign and hands it to the client,
// which connects with it in the relay URL (?jwt=…). The relay checks it with
// Verify. The app decides who may do what; this package is the machinery: the
// key format, the signature, the time claims and confining every path to the
// key's prefix.
//
// A token's expiry decides whether a session may start, not how long it
// lives: a relay checks a live session's token with VerifyLive, which ignores
// the expiry, so the session ends only when the key that signed it is no
// longer trusted.
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
	"slices"
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
var allowedClaims = map[string]bool{
	"path_auth": true, "root": true,
	"publish": true, "subscribe": true, "create": true, "post": true, "redact": true,
	"sub": true, "iat": true, "nbf": true, "exp": true, "jti": true,
}

// grantClaims are the claims of a grant other than path_auth and sub: a token
// carrying any of them carries no path_auth.
var grantClaims = []string{"root", "publish", "subscribe", "create", "post", "redact"}

// Claims are a verified token's: its grant, normalized and confined to its
// key's prefix, when it expires, its jti if any, the key that signed it, and
// whether it is a ticket (SignTicket).
type Claims struct {
	Grant
	ExpiresAt time.Time
	ID        string
	Key       Key
	Ticket    bool
}

// TicketType is the typ header of a ticket (SignTicket), after RFC 8725's
// explicit typing: it marks a token meant only to add its grant to a live
// session, so a relay refuses one presented at connect.
const TicketType = "ticket+jwt"

// jwtType is the typ header of every other token Sign writes.
const jwtType = "JWT"

// pathAuth is the capability claim of other MoQ implementations, per the
// @moq/token convention: root plus pub or sub. A nil Pub or Sub grants
// nothing for that role. Verify reads it; Sign never writes it.
type pathAuth struct {
	Root string  `json:"root"`
	Pub  *string `json:"pub,omitempty"`
	Sub  *string `json:"sub,omitempty"`
}

// claims is a token's claim set as encoded. Numeric dates are float64 on
// decode because JSON numbers may carry a fraction.
type claims struct {
	PathAuth  *pathAuth `json:"path_auth,omitzero"`
	Root      string    `json:"root,omitempty"`
	Publish   []string  `json:"publish,omitempty"`
	Subscribe []string  `json:"subscribe,omitempty"`
	Create    []string  `json:"create,omitempty"`
	Post      []string  `json:"post,omitempty"`
	Redact    []string  `json:"redact,omitempty"`
	Subject   string    `json:"sub,omitzero"`
	IssuedAt  *float64  `json:"iat"`
	NotBefore *float64  `json:"nbf"`
	ExpiresAt *float64  `json:"exp"`
	ID        string    `json:"jti,omitempty"`
}

// grant is the grant of the claims other than path_auth.
func (c claims) grant() Grant {
	return Grant{
		Root: c.Root, Publish: c.Publish, Subscribe: c.Subscribe,
		Create: c.Create, Post: c.Post, Redact: c.Redact, Subject: c.Subject,
	}
}

// Sign returns a token signed by key that grants g for ttl from now, at most
// MaxLifetime: its root, normalized, each action's patterns, and sub when g
// names a subject. It carries a random jti. A grant Verify would refuse, such
// as a root outside key's prefix or a pattern that doesn't parse, is refused
// here. A grant of no patterns names the key's tenant alone.
func Sign(key SigningKey, g Grant, ttl time.Duration) (string, error) {
	return signAt(key, g, ttl, time.Now(), jwtType)
}

// SignTicket signs g as Sign does, as a ticket: a token a client presents on
// a session it already holds, such as on a MoQ Auth Stream, to add g to it
// until the ticket expires. A relay refuses a ticket as the credential a
// session connects with, where its grant would outlive its expiry.
func SignTicket(key SigningKey, g Grant, ttl time.Duration) (string, error) {
	return signAt(key, g, ttl, time.Now(), TicketType)
}

func signAt(key SigningKey, g Grant, ttl time.Duration, now time.Time, typ string) (string, error) {
	if ttl <= 0 || ttl > MaxLifetime {
		return "", fmt.Errorf("token: ttl %s must be positive and at most %s", ttl, MaxLifetime)
	}
	g, err := g.checked(key.Prefix)
	if err != nil {
		return "", err
	}
	c := claims{
		Root: g.Root, Publish: g.Publish, Subscribe: g.Subscribe,
		Create: g.Create, Post: g.Post, Redact: g.Redact,
		Subject: g.Subject, ID: rand.Text(),
	}
	iat := float64(now.Unix())
	exp := float64(now.Add(ttl).Unix())
	c.IssuedAt, c.NotBefore, c.ExpiresAt = &iat, &iat, &exp

	header, err := json.Marshal(map[string]string{"alg": "EdDSA", "kid": key.ID, "typ": typ})
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
// within the allowed set (path_auth, or root, the actions' patterns and an
// optional sub; iat, nbf, exp and an optional jti) with no duplicate members;
// exp, nbf and iat hold within Leeway and the lifetime is at most
// MaxLifetime; the root lies within its key's prefix; and every pattern
// parses as moqt.ParseGrantPattern reads it, with no "." or ".." segment. A
// path_auth claim is returned as the grant it amounts to (see grantOf).
// An error wraps ErrInvalid or ErrForbidden.
func Verify(token string, keys map[string]Key, now time.Time) (Claims, error) {
	return verify(token, keys, now, true)
}

// VerifyLive checks token as the credential of a live session, at now: as
// Verify does, except that a token past its expiry is not refused for it. Its
// expiry decided whether the session could start; the key that signed it must
// still be trusted, and its grant still within the key's prefix.
func VerifyLive(token string, keys map[string]Key, now time.Time) (Claims, error) {
	return verify(token, keys, now, false)
}

// verify is Verify, refusing a token past its expiry only when expiry is set.
func verify(token string, keys map[string]Key, now time.Time, expiry bool) (Claims, error) {
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
	exp, err := c.checkTime(now, expiry)
	if err != nil {
		return Claims{}, err
	}
	_, hasPathAuth := members["path_auth"]
	_, hasSubject := members["sub"]
	hasGrant := slices.ContainsFunc(grantClaims, func(name string) bool {
		_, ok := members[name]
		return ok
	})
	var g Grant
	switch {
	case hasPathAuth && hasGrant:
		return Claims{}, invalid("a token carries path_auth or root and patterns, not both")
	case !hasPathAuth:
		g, err = c.grant().checked(key.Prefix)
	case c.PathAuth == nil:
		return Claims{}, invalid("path_auth is null")
	case hasSubject:
		return Claims{}, invalid("sub goes with root and patterns, not with path_auth")
	default:
		g, err = grantOf(*c.PathAuth, key.Prefix)
	}
	if err != nil {
		return Claims{}, err
	}
	return Claims{Grant: g, ExpiresAt: exp, ID: c.ID, Key: key, Ticket: isTicket(header.Typ)}, nil
}

// grantOf reads path_auth as a grant: its root as the root, pub as publish,
// and sub as subscribe, each on its path and every path beneath it,
// or on everything beneath the root for an empty one. The root must lie
// within prefix at a "/" boundary: the rule that stops a key from signing for
// another tenant's paths.
func grantOf(pa pathAuth, prefix string) (Grant, error) {
	if pa.Pub == nil && pa.Sub == nil {
		return Grant{}, forbidden("path_auth grants neither pub nor sub")
	}
	root, err := normalizePath(pa.Root)
	if err != nil {
		return Grant{}, invalid("path_auth root: %v", err)
	}
	if !within(root, prefix) {
		return Grant{}, forbidden("path_auth root %q lies outside the signing key's prefix %q", root, prefix)
	}
	g := Grant{Root: root}
	if pa.Pub != nil {
		if g.Publish, err = atAndBeneath(*pa.Pub); err != nil {
			return Grant{}, invalid("path_auth pub: %v", err)
		}
	}
	if pa.Sub != nil {
		if g.Subscribe, err = atAndBeneath(*pa.Sub); err != nil {
			return Grant{}, invalid("path_auth sub: %v", err)
		}
	}
	return g, nil
}

// checkTime enforces exp (when expiry is set), nbf and iat, all required,
// with Leeway, and the lifetime cap, and returns exp.
func (c claims) checkTime(now time.Time, expiry bool) (time.Time, error) {
	if c.ExpiresAt == nil || c.NotBefore == nil || c.IssuedAt == nil {
		return time.Time{}, invalid("exp, nbf and iat are required")
	}
	exp, nbf, iat := numericDate(*c.ExpiresAt), numericDate(*c.NotBefore), numericDate(*c.IssuedAt)
	if lifetime := exp.Sub(iat); lifetime <= 0 || lifetime > MaxLifetime {
		return time.Time{}, invalid("lifetime (exp - iat) must be positive and at most %s", MaxLifetime)
	}
	// Judged on whole seconds, the precision of exp as Sign writes it.
	switch {
	case expiry && !now.Before(time.Unix(exp.Add(Leeway).Unix(), 0)):
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

// isTicket reports whether typ marks a ticket. RFC 7515 4.1.9 compares a typ
// ignoring case, and with or without its "application/" prefix.
func isTicket(typ string) bool {
	const prefix = "application/"
	if len(typ) > len(prefix) && strings.EqualFold(typ[:len(prefix)], prefix) {
		typ = typ[len(prefix):]
	}
	return strings.EqualFold(typ, TicketType)
}
