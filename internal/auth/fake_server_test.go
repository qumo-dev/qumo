package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// fakeAuthServer is an auth server whose answer is set by its fields: status
// (200 when zero) and body. It records every request it decodes.
type fakeAuthServer struct {
	status int
	body   string

	mu       sync.Mutex
	requests []Request
}

func (f *fakeAuthServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
		f.mu.Lock()
		f.requests = append(f.requests, req)
		f.mu.Unlock()
	}
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(f.body))
}

// received returns a copy of the requests seen so far.
func (f *fakeAuthServer) received() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

// start serves f on a loopback httptest server for the test's life and
// returns a Client pointed at it.
func (f *fakeAuthServer) start(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	endpoint, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	return &Client{endpoint: endpoint, client: srv.Client()}
}
