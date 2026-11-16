package reqsign_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/reqsign"
	"github.com/qumo-dev/qumo/internal/reqsign/reqsigntest"
)

// verified is what a test server saw: the timestamp a request's signature
// verified with, or why it did not.
type verified struct {
	timestamp int64
	err       error
}

// A signed request verifies at the server as it arrives, whatever its
// method, query or body. The server only reports; the test's goroutine
// asserts.
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
			signer := reqsigntest.Signer(t)
			seen := make(chan verified, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					seen <- verified{err: err}
					return
				}
				ts, err := reqsigntest.Verify(r, body)
				seen <- verified{timestamp: ts, err: err}
			}))
			defer srv.Close()
			req, err := http.NewRequestWithContext(t.Context(), tt.method, srv.URL+tt.path, bytes.NewReader(tt.body))
			require.NoError(t, err)

			require.NoError(t, signer.Sign(req, tt.body))
			resp, err := srv.Client().Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())

			got := <-seen
			require.NoError(t, got.err)
			assert.InDelta(t, time.Now().Unix(), got.timestamp, 5)
		})
	}
}

// A request sent again is signed again, with the time it is sent at: a
// retry is not refused for the age of the first attempt.
func TestSigner_Sign_ARetryIsSignedAgain(t *testing.T) {
	signer := reqsigntest.Signer(t)
	now := time.Unix(1_800_000_000, 0)
	signer.SetNow(func() time.Time { return now })
	body := []byte(`[]`)
	send := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/usage/events", bytes.NewReader(body))
		require.NoError(t, signer.Sign(req, body))
		return req
	}

	first := send()
	now = now.Add(5 * time.Minute)
	retry := send()

	firstTS, err := reqsigntest.Verify(first, body)
	require.NoError(t, err)
	retryTS, err := reqsigntest.Verify(retry, body)
	require.NoError(t, err)
	assert.Equal(t, int64(1_800_000_000), firstTS)
	assert.Equal(t, int64(1_800_000_300), retryTS)
}

// What is signed is bound: a request changed in any part fails the check.
func TestSigner_Sign_BindsTheRequest(t *testing.T) {
	signed := []byte(`[{"egress":1}]`)
	tests := map[string]struct {
		change func(*http.Request) []byte // returns the body received
	}{
		"another body":      {change: func(*http.Request) []byte { return []byte(`[{"egress":2}]`) }},
		"another method":    {change: func(r *http.Request) []byte { r.Method = http.MethodPut; return signed }},
		"another path":      {change: func(r *http.Request) []byte { r.URL.Path = "/v1/relays/trust"; return signed }},
		"another query":     {change: func(r *http.Request) []byte { r.URL.RawQuery = "x=2"; return signed }},
		"another timestamp": {change: func(r *http.Request) []byte { r.Header.Set(reqsign.HeaderTimestamp, "1"); return signed }},
		"another certificate": {change: func(r *http.Request) []byte {
			other := httptest.NewRequest(http.MethodGet, "/", nil)
			require.NoError(t, reqsigntest.Signer(t).Sign(other, nil))
			r.Header.Set(reqsign.HeaderCertificate, other.Header.Get(reqsign.HeaderCertificate))
			return signed
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/usage/events?x=1", nil)
			require.NoError(t, reqsigntest.Signer(t).Sign(req, signed))
			_, err := reqsigntest.Verify(req, signed)
			require.NoError(t, err, "unchanged, it verifies")

			body := tt.change(req)
			_, err = reqsigntest.Verify(req, body)

			assert.Error(t, err)
		})
	}
}

// Without a peer identity nothing is signed.
func TestSigner_Sign_Nil(t *testing.T) {
	var signer *reqsign.Signer
	req := httptest.NewRequest(http.MethodGet, "/v1/relays/trust", nil)

	require.NoError(t, signer.Sign(req, nil))

	assert.Empty(t, req.Header.Get(reqsign.HeaderCertificate))
	assert.Empty(t, req.Header.Get(reqsign.HeaderTimestamp))
	assert.Empty(t, req.Header.Get(reqsign.HeaderSignature))
}

func TestNew(t *testing.T) {
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	tests := map[string]struct {
		pair    func(tls.Certificate) tls.Certificate
		wantErr string
	}{
		"an ECDSA peer identity": {pair: func(p tls.Certificate) tls.Certificate { return p }},
		"a key that is not ECDSA": {
			pair:    func(p tls.Certificate) tls.Certificate { p.PrivateKey = ed; return p },
			wantErr: "not ECDSA",
		},
		"no certificate": {
			pair:    func(p tls.Certificate) tls.Certificate { p.Certificate = nil; return p },
			wantErr: "no certificate",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			signer, err := reqsign.New(tt.pair(reqsigntest.Pair(t)))

			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				assert.Nil(t, signer)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, signer)
		})
	}
}

// Load reads the pair as PEER_CERT_FILE and PEER_KEY_FILE name it.
func TestLoad(t *testing.T) {
	certFile, keyFile := writePair(t, reqsigntest.Pair(t))
	tests := map[string]struct {
		cert, key string
		wantErr   bool
	}{
		"both files":      {cert: certFile, key: keyFile},
		"no key file":     {cert: certFile, key: filepath.Join(t.TempDir(), "missing.key"), wantErr: true},
		"the key as cert": {cert: keyFile, key: keyFile, wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			signer, err := reqsign.Load(tt.cert, tt.key)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodGet, "/v1/relays/trust", nil)
			require.NoError(t, signer.Sign(req, nil))
			_, err = reqsigntest.Verify(req, nil)
			assert.NoError(t, err)
		})
	}
}

// writePair writes pair as PEM files and returns their paths.
func writePair(tb testing.TB, pair tls.Certificate) (certFile, keyFile string) {
	tb.Helper()
	dir := tb.TempDir()
	der, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	require.NoError(tb, err)
	certFile, keyFile = filepath.Join(dir, "peer.crt"), filepath.Join(dir, "peer.key")
	require.NoError(tb, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}), 0o600))
	require.NoError(tb, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600))
	return certFile, keyFile
}
