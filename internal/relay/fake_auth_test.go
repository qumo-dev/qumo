package relay

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/qumo-dev/qumo/internal/auth"
)

// fakeAuth answers every session through its authorize method, which is what
// Server.authorize takes: err when set, otherwise the grant in body (JSON, as
// the auth server sends it). The zero value admits every session with a grant
// that covers nothing. It records every request.
type fakeAuth struct {
	body string
	err  error

	mu       sync.Mutex
	requests []auth.Request
}

func (f *fakeAuth) authorize(_ context.Context, req auth.Request) (*auth.Grant, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var g auth.Grant
	if f.body == "" {
		return &g, nil
	}
	if err := json.Unmarshal([]byte(f.body), &g); err != nil {
		return nil, err
	}
	return &g, nil
}

// received returns a copy of the requests seen so far.
func (f *fakeAuth) received() []auth.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]auth.Request(nil), f.requests...)
}

// allowAll admits every session to everything. Tests and benchmarks of a
// relay whose admission isn't under test pass it as Server.authorize; a
// Server without one refuses every client session.
func allowAll(context.Context, auth.Request) (*auth.Grant, error) {
	var g auth.Grant
	if err := json.Unmarshal([]byte(`{"publish":["**"],"subscribe":["**"]}`), &g); err != nil {
		return nil, err
	}
	return &g, nil
}
