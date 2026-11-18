package reqsign_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
		change func(*testing.T, *http.Request) []byte // returns the body received
	}{
		"another body":      {change: func(_ *testing.T, _ *http.Request) []byte { return []byte(`[{"egress":2}]`) }},
		"another method":    {change: func(_ *testing.T, r *http.Request) []byte { r.Method = http.MethodPut; return signed }},
		"another path":      {change: func(_ *testing.T, r *http.Request) []byte { r.URL.Path = "/v1/relays/trust"; return signed }},
		"another query":     {change: func(_ *testing.T, r *http.Request) []byte { r.URL.RawQuery = "x=2"; return signed }},
		"another timestamp": {change: func(_ *testing.T, r *http.Request) []byte { r.Header.Set(reqsign.HeaderTimestamp, "1"); return signed }},
		"another certificate": {change: func(t *testing.T, r *http.Request) []byte {
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

			body := tt.change(t, req)
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

// A run of 401s is logged once, with both clocks, and its end once: not at
// every retry.
func TestRefusals_Observe(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	answer := func(status int) *http.Response {
		return &http.Response{StatusCode: status, Header: http.Header{"Date": []string{"Mon, 16 Nov 2026 15:49:03 GMT"}}}
	}
	var r reqsign.Refusals

	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusUnauthorized, http.StatusUnauthorized, http.StatusServiceUnavailable, http.StatusOK, http.StatusUnauthorized} {
		r.Observe("https://cp.example.com/v1/relays/trust", answer(status))
	}

	out := logs.String()
	assert.Equal(t, 2, strings.Count(out, "refused the relay's request (401)"), "the first 401 of each run")
	assert.Equal(t, 1, strings.Count(out, "answers the relay's requests again"), "the end of the first run")
	assert.Contains(t, out, `server_date="Mon, 16 Nov 2026 15:49:03 GMT"`)
}

// The shared client answers a redirect itself and never follows it.
func TestNoRedirectClient(t *testing.T) {
	var followed atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Store(true) }))
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	resp, err := reqsign.NoRedirectClient(time.Second).Do(req)

	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
	assert.False(t, followed.Load())
}

// Rotate makes the signer sign with the new pair, its certificate sent and
// its key's signature verifying; a pair it can't sign with leaves it as it
// was.
func TestSigner_Rotate(t *testing.T) {
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	unsignable := reqsigntest.Pair(t)
	unsignable.PrivateKey = ed
	tests := map[string]struct {
		pair     tls.Certificate
		wantErr  string
		wantSent func(old, next tls.Certificate) tls.Certificate
	}{
		"a renewed pair":         {pair: reqsigntest.Pair(t), wantSent: func(_, next tls.Certificate) tls.Certificate { return next }},
		"a key that isn't ECDSA": {pair: unsignable, wantErr: "not ECDSA", wantSent: func(old, _ tls.Certificate) tls.Certificate { return old }},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			old := reqsigntest.Pair(t)
			signer, err := reqsign.New(old)
			require.NoError(t, err)

			err = signer.Rotate(tt.pair)

			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			req := httptest.NewRequest(http.MethodGet, "/v1/relays/trust", nil)
			require.NoError(t, signer.Sign(req, nil))
			_, err = reqsigntest.Verify(req, nil)
			assert.NoError(t, err, "the signature verifies with the certificate sent")
			signed, err := reqsign.New(tt.wantSent(old, tt.pair))
			require.NoError(t, err)
			want := httptest.NewRequest(http.MethodGet, "/v1/relays/trust", nil)
			require.NoError(t, signed.Sign(want, nil))
			assert.Equal(t, want.Header.Get(reqsign.HeaderCertificate), req.Header.Get(reqsign.HeaderCertificate), "the certificate sent")
		})
	}
}
func TestCheckURL(t *testing.T) {
	tests := map[string]struct {
		url         string
		wantErrText string
	}{
		"https":             {url: "https://keys.example.com/v1/set"},
		"loopback http":     {url: "http://127.0.0.1:4440/"},
		"localhost http":    {url: "http://localhost:4440/"},
		"http off loopback": {url: "http://keys.example.com/", wantErrText: "loopback"},
		"another scheme":    {url: "ftp://keys.example.com/", wantErrText: "https"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := reqsign.CheckURL(tt.url)

			if tt.wantErrText != "" {
				assert.ErrorContains(t, err, tt.wantErrText)
				return
			}
			assert.NoError(t, err)
		})
	}
}
