//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/qumo-dev/qumo/internal/auth"
)

// fakeAuth answers every session through its authorize method, which is what
// relay.Server.Authorize takes: err when set, otherwise the grant in body
// (JSON, as an auth server sends it). The zero value admits every session
// with a grant that covers nothing. It records every request.
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

// authOff admits every session unchecked, as a relay with auth off does
// (QUMO_AUTH_URL unset): a nil grant with a nil error.
func authOff(context.Context, auth.Request) (*auth.Grant, error) {
	return nil, nil
}

// nativeURL is the native-QUIC (moqt://) URL of a relay at addr.
func nativeURL(addr string) string {
	return "moqt://" + addr
}
