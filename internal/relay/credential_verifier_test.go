package relay

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testIssuer      = "https://api.example.com"
	testDevAudience = "qumo-relay-dev"
	testKID         = "kid-1"
)

// testNow is the fixed clock every verifier test runs at.
var testNow = time.Unix(1_800_000_000, 0)

// testSigner is an Ed25519 key pair standing in for the control plane's
// signing key.
type testSigner struct {
	kid  string
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func newTestSigner(tb testing.TB, kid string) testSigner {
	tb.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(tb, err)
	return testSigner{kid: kid, priv: priv, pub: pub}
}

// sign builds a compact JWS with the given header and claims, signed by s.
func (s testSigner) sign(tb testing.TB, header, claims map[string]any) string {
	tb.Helper()
	h, err := json.Marshal(header)
	require.NoError(tb, err)
	c, err := json.Marshal(claims)
	require.NoError(tb, err)
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.priv, []byte(input)))
}

// validClaims are the claims qumo-deploy issues to a dev-project key that
// publishes under tenant/project/live, valid at testNow.
func validClaims() map[string]any {
	return map[string]any{
		"jti":       "jti-1",
		"sub":       "key-1",
		"iss":       testIssuer,
		"aud":       testDevAudience,
		"iat":       testNow.Add(-time.Minute).Unix(),
		"nbf":       testNow.Add(-time.Minute).Unix(),
		"exp":       testNow.Add(10 * time.Minute).Unix(),
		"path_auth": map[string]any{"root": "tenant/project", "pub": "live"},
	}
}

func newTestVerifier(s testSigner) *localVerifier {
	return &localVerifier{
		keys:     &fakeSigningKeys{keys: map[string]ed25519.PublicKey{s.kid: s.pub}},
		issuer:   testIssuer,
		audience: testDevAudience,
		now:      func() time.Time { return testNow },
	}
}

func TestLocalVerifier_Verify(t *testing.T) {
	signer := newTestSigner(t, testKID)
	other := newTestSigner(t, testKID) // same kid, different key: a forgery
	eddsa := map[string]any{"alg": "EdDSA", "kid": testKID, "typ": "JWT"}
	with := func(key string, value any) map[string]any {
		c := validClaims()
		if value == nil {
			delete(c, key)
		} else {
			c[key] = value
		}
		return c
	}

	tests := map[string]struct {
		token   string
		path    string
		wantErr error
	}{
		"valid dev credential":              {token: signer.sign(t, eddsa, validClaims()), path: "/tenant/project/live"},
		"path beneath the grant":            {token: signer.sign(t, eddsa, validClaims()), path: "/tenant/project/live/cam1"},
		"aud as an array":                   {token: signer.sign(t, eddsa, with("aud", []string{"other", testDevAudience})), path: "/tenant/project/live"},
		"issuer with a trailing slash":      {token: signer.sign(t, eddsa, with("iss", testIssuer+"/")), path: "/tenant/project/live"},
		"expired within the leeway":         {token: signer.sign(t, eddsa, with("exp", testNow.Add(-30*time.Second).Unix())), path: "/tenant/project/live"},
		"managed audience on a dev relay":   {token: signer.sign(t, eddsa, with("aud", managedAudience)), path: "/tenant/project/live", wantErr: errWrongAudience},
		"no audience (issued before #1153)": {token: signer.sign(t, eddsa, with("aud", nil)), path: "/tenant/project/live", wantErr: errWrongAudience},
		"wrong issuer":                      {token: signer.sign(t, eddsa, with("iss", "https://evil.example.com")), path: "/tenant/project/live", wantErr: errWrongIssuer},
		"expired beyond the leeway":         {token: signer.sign(t, eddsa, with("exp", testNow.Add(-2*time.Minute).Unix())), path: "/tenant/project/live", wantErr: errExpired},
		"not yet valid":                     {token: signer.sign(t, eddsa, with("nbf", testNow.Add(2*time.Minute).Unix())), path: "/tenant/project/live", wantErr: errNotYetValid},
		"issued in the future":              {token: signer.sign(t, eddsa, with("iat", testNow.Add(2*time.Minute).Unix())), path: "/tenant/project/live", wantErr: errNotYetValid},
		"no exp":                            {token: signer.sign(t, eddsa, with("exp", nil)), path: "/tenant/project/live", wantErr: errMalformedCredential},
		"no jti":                            {token: signer.sign(t, eddsa, with("jti", nil)), path: "/tenant/project/live", wantErr: errMalformedCredential},
		"sibling path sharing a prefix":     {token: signer.sign(t, eddsa, validClaims()), path: "/tenant/project/livestream", wantErr: errPathNotCovered},
		"subscribe-only grant":              {token: signer.sign(t, eddsa, with("path_auth", map[string]any{"root": "tenant/project", "sub": "live"})), path: "/tenant/project/live", wantErr: errPathNotCovered},
		"forged signature":                  {token: other.sign(t, eddsa, validClaims()), path: "/tenant/project/live", wantErr: errBadSignature},
		"unknown kid":                       {token: signer.sign(t, map[string]any{"alg": "EdDSA", "kid": "kid-2"}, validClaims()), path: "/tenant/project/live", wantErr: errUnknownKey},
		"HS256 header":                      {token: signer.sign(t, map[string]any{"alg": "HS256", "kid": testKID}, validClaims()), path: "/tenant/project/live", wantErr: errUnsupportedAlg},
		"no kid":                            {token: signer.sign(t, map[string]any{"alg": "EdDSA"}, validClaims()), path: "/tenant/project/live", wantErr: errMalformedCredential},
		"not a JWS":                         {token: "header.payload", path: "/tenant/project/live", wantErr: errMalformedCredential},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			jti, err := newTestVerifier(signer).verify(t.Context(), tt.token, tt.path)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				assert.Empty(t, jti)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "jti-1", jti)
		})
	}
}

func TestLocalVerifier_Verify_TamperedClaims(t *testing.T) {
	signer := newTestSigner(t, testKID)
	token := signer.sign(t, map[string]any{"alg": "EdDSA", "kid": testKID}, validClaims())

	// Widen the grant after signing: the signature no longer covers the claims.
	parts := strings.Split(token, ".")
	widened := validClaims()
	widened["path_auth"] = map[string]any{"root": "", "pub": ""}
	raw, err := json.Marshal(widened)
	require.NoError(t, err)
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(raw) + "." + parts[2]

	_, err = newTestVerifier(signer).verify(t.Context(), tampered, "/other-tenant/x")
	assert.ErrorIs(t, err, errBadSignature)
}

func TestLocalVerifier_Verify_KeysUnavailable(t *testing.T) {
	signer := newTestSigner(t, testKID)
	token := signer.sign(t, map[string]any{"alg": "EdDSA", "kid": testKID}, validClaims())

	tests := map[string]struct{ err error }{
		"JWKS never fetched": {err: errNoKeys},
		"JWKS too stale":     {err: errKeysStale},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			v := newTestVerifier(signer)
			v.keys = &fakeSigningKeys{err: tt.err}
			_, err := v.verify(t.Context(), token, "/tenant/project/live")
			assert.ErrorIs(t, err, tt.err)
		})
	}
}

// TestCredentialPathAuth_CoversPublish pins the relay to qumo-deploy's rule
// (CredentialClaims.coversPublish): equal to root+pub, or beneath it at a
// segment boundary, after normalizing slashes.
func TestCredentialPathAuth_CoversPublish(t *testing.T) {
	str := func(s string) *string { return &s }
	tests := map[string]struct {
		grant *credentialPathAuth
		path  string
		want  bool
	}{
		"exact":                     {grant: &credentialPathAuth{Root: "t/p", Pub: str("live")}, path: "t/p/live", want: true},
		"leading and double slash":  {grant: &credentialPathAuth{Root: "t/p", Pub: str("live")}, path: "//t/p//live/", want: true},
		"beneath":                   {grant: &credentialPathAuth{Root: "t/p", Pub: str("live")}, path: "t/p/live/a/b", want: true},
		"empty pub grants the root": {grant: &credentialPathAuth{Root: "t/p", Pub: str("")}, path: "t/p/anything", want: true},
		"sibling prefix":            {grant: &credentialPathAuth{Root: "t/p", Pub: str("live")}, path: "t/p/lively", want: false},
		"parent":                    {grant: &credentialPathAuth{Root: "t/p", Pub: str("live")}, path: "t/p", want: false},
		"no publish grant":          {grant: &credentialPathAuth{Root: "t/p"}, path: "t/p/live", want: false},
		"no grant":                  {grant: nil, path: "t/p/live", want: false},
		"empty path":                {grant: &credentialPathAuth{Root: "t/p", Pub: str("")}, path: "/", want: false},
		"empty scope":               {grant: &credentialPathAuth{Root: "", Pub: str("")}, path: "t/p", want: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.grant.coversPublish(tt.path))
		})
	}
}

// jwksServer serves a key set for signers and counts requests. failing makes
// every request answer 503.
type jwksServer struct {
	srv      *httptest.Server
	requests atomic.Int64
	failing  atomic.Bool
	signers  atomic.Pointer[[]testSigner]
}

func newJWKSServer(tb testing.TB, signers ...testSigner) *jwksServer {
	tb.Helper()
	s := &jwksServer{}
	s.signers.Store(&signers)
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		if r.URL.Path != jwksPath || s.failing.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		keys := []map[string]string{
			// A key of another type, which the relay must skip.
			{"kty": "RSA", "kid": "rsa-1", "n": "AQAB", "e": "AQAB"},
		}
		for _, sg := range *s.signers.Load() {
			keys = append(keys, map[string]string{
				"kty": "OKP", "crv": "Ed25519", "kid": sg.kid,
				"x": base64.RawURLEncoding.EncodeToString(sg.pub), "alg": "EdDSA", "use": "sig",
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	tb.Cleanup(s.srv.Close)
	return s
}

// newTestJWKSCache points a cache at s with a controllable clock.
func newTestJWKSCache(s *jwksServer, clock *time.Time) *jwksCache {
	c := newJWKSCache(s.srv.URL, s.srv.Client())
	c.now = func() time.Time { return *clock }
	return c
}

func TestJWKSCache_Key(t *testing.T) {
	signer := newTestSigner(t, testKID)
	srv := newJWKSServer(t, signer)
	clock := testNow
	cache := newTestJWKSCache(srv, &clock)

	_, err := cache.key(t.Context(), testKID)
	require.ErrorIs(t, err, errNoKeys, "nothing is admitted before the first fetch")

	require.NoError(t, cache.refresh(t.Context()))
	pub, err := cache.key(t.Context(), testKID)
	require.NoError(t, err)
	assert.Equal(t, signer.pub, pub)

	_, err = cache.key(t.Context(), "rsa-1")
	assert.ErrorIs(t, err, errUnknownKey, "non-Ed25519 keys are skipped")
}

func TestJWKSCache_Key_UnknownKIDRefetch(t *testing.T) {
	first := newTestSigner(t, "kid-1")
	rotated := newTestSigner(t, "kid-2")
	srv := newJWKSServer(t, first)
	clock := testNow
	cache := newTestJWKSCache(srv, &clock)
	require.NoError(t, cache.refresh(t.Context()))

	// The control plane publishes a new key; an unknown kid refetches at once.
	srv.signers.Store(&[]testSigner{rotated, first})
	pub, err := cache.key(t.Context(), "kid-2")
	require.NoError(t, err)
	assert.Equal(t, rotated.pub, pub)

	// Another unknown kid within the interval does not fetch again.
	before := srv.requests.Load()
	_, err = cache.key(t.Context(), "forged")
	assert.ErrorIs(t, err, errUnknownKey)
	assert.Equal(t, before, srv.requests.Load(), "refetches are rate-limited")

	// After the interval it may fetch once more.
	clock = clock.Add(jwksUnknownKIDInterval)
	_, err = cache.key(t.Context(), "forged")
	assert.ErrorIs(t, err, errUnknownKey)
	assert.Equal(t, before+1, srv.requests.Load())
}

func TestJWKSCache_Key_FailStatic(t *testing.T) {
	signer := newTestSigner(t, testKID)
	srv := newJWKSServer(t, signer)
	clock := testNow
	cache := newTestJWKSCache(srv, &clock)
	require.NoError(t, cache.refresh(t.Context()))

	srv.failing.Store(true)
	clock = clock.Add(jwksMaxStaleness - time.Minute)
	assert.Error(t, cache.refresh(t.Context()))
	_, err := cache.key(t.Context(), testKID)
	require.NoError(t, err, "a failed refresh keeps the cached keys")

	clock = clock.Add(2 * time.Minute)
	_, err = cache.key(t.Context(), testKID)
	assert.ErrorIs(t, err, errKeysStale, "past the staleness cap nothing is admitted")

	srv.failing.Store(false)
	require.NoError(t, cache.refresh(t.Context()))
	_, err = cache.key(t.Context(), testKID)
	assert.NoError(t, err, "a successful refresh resumes admission")
}

func TestJWKSCache_Refresh_Errors(t *testing.T) {
	tests := map[string]struct {
		body   string
		status int
	}{
		"server error":        {status: http.StatusServiceUnavailable},
		"not JSON":            {status: http.StatusOK, body: "<html>"},
		"no usable keys":      {status: http.StatusOK, body: `{"keys":[{"kty":"RSA","kid":"r"}]}`},
		"short Ed25519 key":   {status: http.StatusOK, body: `{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"k","x":"AAAA"}]}`},
		"Ed25519 key, no kid": {status: http.StatusOK, body: fmt.Sprintf(`{"keys":[{"kty":"OKP","crv":"Ed25519","x":%q}]}`, base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize)))},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)
			cache := newJWKSCache(srv.URL, srv.Client())

			err := cache.refresh(t.Context())
			assert.Error(t, err)
			_, err = cache.key(t.Context(), "k")
			assert.ErrorIs(t, err, errNoKeys, "a failed first fetch admits nothing")
		})
	}
}
