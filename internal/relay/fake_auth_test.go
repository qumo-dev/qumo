package relay

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/stretchr/testify/require"
)

// fakeAuth answers every session through its authorize method, which is what
// Server.Authorize takes: err when set, otherwise grant. The zero value admits
// every session with a grant that covers nothing. It records every request.
// block makes every call wait for its context to end, like a stalled check.
type fakeAuth struct {
	grant *auth.Grant
	err   error
	block bool

	// endErr is what end answers.
	endErr error

	mu       sync.Mutex
	requests []auth.Request
	ends     []auth.Request
}

func (f *fakeAuth) authorize(ctx context.Context, req auth.Request) (*auth.Grant, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.grant == nil {
		return &auth.Grant{}, nil
	}
	return f.grant, nil
}

// end records an end report, which is what Server.End takes. It answers
// endErr.
func (f *fakeAuth) end(_ context.Context, req auth.Request) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ends = append(f.ends, req)
	return f.endErr
}

// ended returns a copy of the end reports seen so far.
func (f *fakeAuth) ended() []auth.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]auth.Request(nil), f.ends...)
}

// received returns a copy of the requests seen so far.
func (f *fakeAuth) received() []auth.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]auth.Request(nil), f.requests...)
}

// testGrant returns a grant for one publish and one subscribe pattern ("" for
// none), ending at expires and checked again every revalidate.
func testGrant(tb testing.TB, publish, subscribe string, expires time.Time, revalidate time.Duration) *auth.Grant {
	tb.Helper()
	var pub, sub []string
	if publish != "" {
		pub = []string{publish}
	}
	if subscribe != "" {
		sub = []string{subscribe}
	}
	g, err := auth.NewGrant(pub, sub, expires, revalidate)
	require.NoError(tb, err)
	return g
}
