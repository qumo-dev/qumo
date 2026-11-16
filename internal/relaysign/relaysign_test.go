package relaysign

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testPair is a peer certificate and its ECDSA P-256 key, as the relay CA
// issues them (here self-signed: the chain is the control plane's check, not
// the signature's).
func testPair(tb testing.TB) tls.Certificate {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(tb, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "qumo-relay-tk1a-1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(tb, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// verify checks r as the control plane does (foalk-inc/qumo-deploy#1724),
// written here from the contract rather than shared with it: the
// certificate from its header, and its key's signature over the context,
// method, request URI as received, timestamp and hex SHA-256 of body, one
// per line. It returns the timestamp signed.
func verify(t *testing.T, r *http.Request, body []byte) int64 {
	t.Helper()
	certPEM, err := base64.StdEncoding.DecodeString(r.Header.Get(HeaderCertificate))
	require.NoError(t, err)
	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	sig, err := base64.StdEncoding.DecodeString(r.Header.Get(HeaderSignature))
	require.NoError(t, err)
	sum := sha256.Sum256(body)
	msg := "qumo-relay-request\n" + r.Method + "\n" + r.URL.RequestURI() + "\n" + r.Header.Get(HeaderTimestamp) + "\n" + hex.EncodeToString(sum[:])
	require.NoError(t, cert.CheckSignature(x509.ECDSAWithSHA256, []byte(msg), sig))
	ts, err := strconv.ParseInt(r.Header.Get(HeaderTimestamp), 10, 64)
	require.NoError(t, err)
	return ts
}

// A signed request verifies at the server as it arrives, whatever its
// method, query or body.
func TestSigner_Sign(t *testing.T) {
	tests := map[string]struct {
		method string
		path   string
		body   []byte
	}{
		"a GET with a query":         {method: http.MethodGet, path: "/v1/relays/trust?since=a%2Fb&x=1"},
		"a POST with a body":         {method: http.MethodPost, path: "/v1/usage/events", body: []byte(`[{"type":"session.close"}]`)},
		"a GET of an escaped path":   {method: http.MethodGet, path: "/v1/relays/dns%2Fzone"},
		"a POST with an empty body":  {method: http.MethodPost, path: "/v1/usage/events", body: []byte{}},
		"a path with no query, bare": {method: http.MethodGet, path: "/v1/relays/dns"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			signer, err := New(testPair(t))
			require.NoError(t, err)
			verified := make(chan int64, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				verified <- verify(t, r, body)
			}))
			defer srv.Close()
			req, err := http.NewRequestWithContext(t.Context(), tt.method, srv.URL+tt.path, bytes.NewReader(tt.body))
			require.NoError(t, err)

			require.NoError(t, signer.Sign(req, tt.body))
			resp, err := srv.Client().Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())

			assert.InDelta(t, time.Now().Unix(), <-verified, 5)
		})
	}
}

// A request sent again is signed again, with the time it is sent at: a
// retry is not refused for the age of the first attempt.
func TestSigner_Sign_ARetryIsSignedAgain(t *testing.T) {
	signer, err := New(testPair(t))
	require.NoError(t, err)
	now := time.Unix(1_800_000_000, 0)
	signer.now = func() time.Time { return now }
	body := []byte(`[]`)
	send := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/usage/events", bytes.NewReader(body))
		require.NoError(t, signer.Sign(req, body))
		return req
	}

	first := send()
	now = now.Add(5 * time.Minute)
	retry := send()

	assert.Equal(t, int64(1_800_000_000), verify(t, first, body))
	assert.Equal(t, int64(1_800_000_300), verify(t, retry, body))
}

// A request whose body is not the one signed fails the control plane's check.
func TestSigner_Sign_BindsTheBody(t *testing.T) {
	signer, err := New(testPair(t))
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/usage/events", nil)
	require.NoError(t, signer.Sign(req, []byte(`[{"egress":1}]`)))

	certPEM, err := base64.StdEncoding.DecodeString(req.Header.Get(HeaderCertificate))
	require.NoError(t, err)
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	sig, err := base64.StdEncoding.DecodeString(req.Header.Get(HeaderSignature))
	require.NoError(t, err)

	assert.Error(t, cert.CheckSignature(x509.ECDSAWithSHA256, signed(req.Method, req.URL.RequestURI(), req.Header.Get(HeaderTimestamp), []byte(`[{"egress":2}]`)), sig))
}

// Without a peer identity nothing is signed.
func TestSigner_Sign_Nil(t *testing.T) {
	var signer *Signer
	req := httptest.NewRequest(http.MethodGet, "/v1/relays/trust", nil)

	require.NoError(t, signer.Sign(req, nil))

	assert.Empty(t, req.Header.Get(HeaderCertificate))
	assert.Empty(t, req.Header.Get(HeaderSignature))
}

// Only an ECDSA key signs what the control plane verifies.
func TestNew_RefusesANonECDSAKey(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pair := testPair(t)
	pair.PrivateKey = key

	_, err = New(pair)

	assert.ErrorContains(t, err, "not ECDSA")
}
