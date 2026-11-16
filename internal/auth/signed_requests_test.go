package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/relaysign"
	"github.com/qumo-dev/qumo/token"
)

// testSigner is a relay's peer identity for a test (internal/relaysign
// checks the signature itself).
func testSigner(tb testing.TB) *relaysign.Signer {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(tb, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "qumo-relay-tk1a-1"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(tb, err)
	signer, err := relaysign.New(tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key})
	require.NoError(tb, err)
	return signer
}

// signedHeaders records the signature headers and bearer of each request.
type signedHeaders struct {
	mu   sync.Mutex
	seen []http.Header
}

func (s *signedHeaders) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, r.Header.Clone())
}

func (s *signedHeaders) all() []http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen
}

func assertSigned(t *testing.T, h http.Header, bearer string) {
	t.Helper()
	assert.NotEmpty(t, h.Get(relaysign.HeaderCertificate))
	assert.NotEmpty(t, h.Get(relaysign.HeaderTimestamp))
	assert.NotEmpty(t, h.Get(relaysign.HeaderSignature))
	assert.Equal(t, "Bearer "+bearer, h.Get("Authorization"), "the token is still sent beside the signature")
}

// With a signer, the key-set download and every usage send, a resend after a
// failure included, are signed and still carry the token.
func TestVerifier_SignsItsRequests(t *testing.T) {
	var keyReqs, usageReqs signedHeaders
	k := genKey(t, "acme/app")
	keys := keySetJSON(t, []token.SigningKey{k})
	usageStatus := []int{http.StatusServiceUnavailable, http.StatusOK}
	var usageMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/keys":
			keyReqs.record(r)
			_, _ = w.Write(keys)
		case "/usage":
			usageReqs.record(r)
			usageMu.Lock()
			status := usageStatus[0]
			if len(usageStatus) > 1 {
				usageStatus = usageStatus[1:]
			}
			usageMu.Unlock()
			w.WriteHeader(status)
		}
	}))
	defer srv.Close()
	v, err := NewVerifier(VerifierConfig{Keys: srv.URL + "/keys", UsageURL: srv.URL + "/usage", Token: "relay-token", Signer: testSigner(t)})
	require.NoError(t, err)
	var store keyStore

	require.NoError(t, v.source.refresh(context.Background(), &store, time.Now()))
	v.usage.open("s1", testUsageSession)
	assert.Error(t, v.usage.flush(context.Background()))
	require.NoError(t, v.usage.flush(context.Background()))

	require.Len(t, keyReqs.all(), 1)
	assertSigned(t, keyReqs.all()[0], "relay-token")
	require.Len(t, usageReqs.all(), 2)
	for _, h := range usageReqs.all() {
		assertSigned(t, h, "relay-token")
	}
}

// Without one, nothing is signed: the token alone, as before.
func TestVerifier_UnsignedWithoutASigner(t *testing.T) {
	var reqs signedHeaders
	k := genKey(t, "acme/app")
	keys := keySetJSON(t, []token.SigningKey{k})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.record(r)
		_, _ = w.Write(keys)
	}))
	defer srv.Close()
	v, err := NewVerifier(VerifierConfig{Keys: srv.URL, Token: "relay-token"})
	require.NoError(t, err)
	var store keyStore

	require.NoError(t, v.source.refresh(context.Background(), &store, time.Now()))

	require.Len(t, reqs.all(), 1)
	assert.Empty(t, reqs.all()[0].Get(relaysign.HeaderSignature))
	assert.Equal(t, "Bearer relay-token", reqs.all()[0].Get("Authorization"))
}
