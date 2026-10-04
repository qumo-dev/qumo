package relay

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/qumo-dev/qumo/internal/auth"
)

// fakeAuth answers every session through its authorize method, which is what
// Server.Authorize takes: err when set, otherwise the grant in body (JSON, as
// the auth server sends it). The zero value admits every session with a grant
// that covers nothing. It records every request.
//
// replies, when set, is a results queue that takes the place of body and
// err: one reply per call in order, the last repeating once it is exhausted.
// block makes every call wait for its context to end, like a stalled server.
type fakeAuth struct {
	body    string
	err     error
	replies []fakeReply
	block   bool

	mu       sync.Mutex
	requests []auth.Request
}

// fakeReply is one answer from fakeAuth: err when set, otherwise the grant in
// body.
type fakeReply struct {
	body string
	err  error
}

func (f *fakeAuth) authorize(ctx context.Context, req auth.Request) (*auth.Grant, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	reply := fakeReply{body: f.body, err: f.err}
	if len(f.replies) > 0 {
		reply = f.replies[min(len(f.requests), len(f.replies))-1]
	}
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if reply.err != nil {
		return nil, reply.err
	}
	var g auth.Grant
	if reply.body == "" {
		return &g, nil
	}
	if err := json.Unmarshal([]byte(reply.body), &g); err != nil {
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
