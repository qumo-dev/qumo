package token

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json/v2"
	"testing"
	"time"
)

// rawSigner mints tokens whose header and claims a test sets freely, to
// exercise Verify's refusals. The zero value is usable: its key pair is
// generated on first use, and prefix is what the trusted key is confined to.
type rawSigner struct {
	prefix  string
	private ed25519.PrivateKey
}

// trusted is the Key a verifier holds for this signer.
func (s *rawSigner) trusted(tb testing.TB) Key {
	tb.Helper()
	if s.private == nil {
		_, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			tb.Fatalf("generate key: %v", err)
		}
		s.private = priv
	}
	pub := s.private.Public().(ed25519.PublicKey)
	return Key{ID: thumbprint(pub), Public: pub, Prefix: s.prefix}
}

// keys is a trusted key set holding only this signer's key.
func (s *rawSigner) keys(tb testing.TB) map[string]Key {
	tb.Helper()
	k := s.trusted(tb)
	return map[string]Key{k.ID: k}
}

// sign mints a token with this signer's kid and the given claims.
func (s *rawSigner) sign(tb testing.TB, claims map[string]any) string {
	tb.Helper()
	return s.signWithHeader(tb, map[string]any{"alg": "EdDSA", "kid": s.trusted(tb).ID, "typ": "JWT"}, claims)
}

// signWithHeader mints a token with an arbitrary header, signed by this
// signer's key whatever the header claims.
func (s *rawSigner) signWithHeader(tb testing.TB, header, claims map[string]any) string {
	tb.Helper()
	return s.signSegments(tb, encodeJSON(tb, header), encodeJSON(tb, claims))
}

// signWithRawHeader mints a token with valid claims at now whose header is the
// JSON text rawHeader, so a test can sign a header a map can't express (a
// member given twice, or one that isn't an object).
func (s *rawSigner) signWithRawHeader(tb testing.TB, rawHeader string, now time.Time) string {
	tb.Helper()
	return s.signSegments(tb, encodeRaw(tb, rawHeader), encodeJSON(tb, validClaims(now)))
}

// signWithRawClaims mints a token whose claims segment is rawClaims, already
// base64url-encoded, so a test can sign JSON a map can't express (a duplicate
// member).
func (s *rawSigner) signWithRawClaims(tb testing.TB, rawClaims string) string {
	tb.Helper()
	return s.signSegments(tb, encodeJSON(tb, map[string]any{"alg": "EdDSA", "kid": s.trusted(tb).ID}), rawClaims)
}

// signSegments signs a header and a claims segment, each already
// base64url-encoded, with this signer's key.
func (s *rawSigner) signSegments(tb testing.TB, header, claims string) string {
	tb.Helper()
	s.trusted(tb)
	input := header + "." + claims
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.private, []byte(input)))
}

// encodeJSON encodes v as JSON, base64url-encoded for a token segment.
func encodeJSON(tb testing.TB, v any) string {
	tb.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		tb.Fatalf("encode: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// encodeRaw base64url-encodes raw JSON for a token segment.
func encodeRaw(tb testing.TB, raw string) string {
	tb.Helper()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// validClaims are the claims of a current token granting publish under
// acme/app/alice and subscribe under acme/app, issued at now for 10 minutes.
func validClaims(now time.Time) map[string]any {
	return map[string]any{
		"path_auth": map[string]any{"root": "acme/app", "pub": "alice", "sub": ""},
		"iat":       now.Unix(),
		"nbf":       now.Unix(),
		"exp":       now.Add(10 * time.Minute).Unix(),
		"jti":       "j-1",
	}
}
