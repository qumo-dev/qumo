package credential

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// newTestJWKS points a cache at s with a controllable clock.
func newTestJWKS(s *jwksServer, clock *time.Time) *JWKS {
	c := NewJWKS(s.srv.URL, s.srv.Client())
	c.now = func() time.Time { return *clock }
	return c
}

func TestJWKS_Key(t *testing.T) {
	signer := newTestSigner(t, testKID)
	srv := newJWKSServer(t, signer)
	clock := testNow
	cache := newTestJWKS(srv, &clock)

	_, err := cache.Key(t.Context(), testKID)
	require.ErrorIs(t, err, errNoKeys, "nothing is admitted before the first fetch")

	require.NoError(t, cache.refresh(t.Context()))
	pub, err := cache.Key(t.Context(), testKID)
	require.NoError(t, err)
	assert.Equal(t, signer.pub, pub)

	_, err = cache.Key(t.Context(), "rsa-1")
	assert.ErrorIs(t, err, errUnknownKey, "non-Ed25519 keys are skipped")
}

func TestJWKS_Key_UnknownKIDRefetch(t *testing.T) {
	first := newTestSigner(t, "kid-1")
	rotated := newTestSigner(t, "kid-2")
	srv := newJWKSServer(t, first)
	clock := testNow
	cache := newTestJWKS(srv, &clock)
	require.NoError(t, cache.refresh(t.Context()))

	// The control plane publishes a new key; an unknown kid refetches at once.
	srv.signers.Store(&[]testSigner{rotated, first})
	pub, err := cache.Key(t.Context(), "kid-2")
	require.NoError(t, err)
	assert.Equal(t, rotated.pub, pub)

	// Another unknown kid within the interval does not fetch again.
	before := srv.requests.Load()
	_, err = cache.Key(t.Context(), "forged")
	assert.ErrorIs(t, err, errUnknownKey)
	assert.Equal(t, before, srv.requests.Load(), "refetches are rate-limited")

	// After the interval it may fetch once more.
	clock = clock.Add(jwksUnknownKIDInterval)
	_, err = cache.Key(t.Context(), "forged")
	assert.ErrorIs(t, err, errUnknownKey)
	assert.Equal(t, before+1, srv.requests.Load())
}

func TestJWKS_Key_FailStatic(t *testing.T) {
	signer := newTestSigner(t, testKID)
	srv := newJWKSServer(t, signer)
	clock := testNow
	cache := newTestJWKS(srv, &clock)
	require.NoError(t, cache.refresh(t.Context()))

	srv.failing.Store(true)
	clock = clock.Add(jwksMaxStaleness - time.Minute)
	assert.Error(t, cache.refresh(t.Context()))
	_, err := cache.Key(t.Context(), testKID)
	require.NoError(t, err, "a failed refresh keeps the cached keys")

	clock = clock.Add(2 * time.Minute)
	_, err = cache.Key(t.Context(), testKID)
	assert.ErrorIs(t, err, errKeysStale, "past the staleness cap nothing is admitted")

	srv.failing.Store(false)
	require.NoError(t, cache.refresh(t.Context()))
	_, err = cache.Key(t.Context(), testKID)
	assert.NoError(t, err, "a successful refresh resumes admission")
}

func TestJWKS_Refresh_Errors(t *testing.T) {
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
			cache := NewJWKS(srv.URL, srv.Client())

			err := cache.refresh(t.Context())
			assert.Error(t, err)
			_, err = cache.Key(t.Context(), "k")
			assert.ErrorIs(t, err, errNoKeys, "a failed first fetch admits nothing")
		})
	}
}
