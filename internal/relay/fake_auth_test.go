//go:build integration

package relay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/qumo-dev/qumo/internal/auth"
)

// fakeAuth answers every session as an auth server would: refused with
// status when it is 401 or 403, unavailable for any other non-zero status,
// and otherwise admitted with the grant in body (JSON, as the auth server
// sends it). It records every request. Its authorize method is what a
// Server's authorize field takes.
type fakeAuth struct {
	status int
	body   string

	mu       sync.Mutex
	requests []auth.Request
}

func (f *fakeAuth) authorize(_ context.Context, req auth.Request) (*auth.Grant, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	switch f.status {
	case 0:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, auth.RefusedError{Status: f.status}
	default:
		return nil, errors.New("auth server unavailable")
	}
	var g auth.Grant
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
