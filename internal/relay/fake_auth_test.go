package relay

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/token"
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

// testGrant returns a grant that may publish beneath one subtree pattern and
// subscribe to and fetch beneath another ("**" for everything, "a/b/**" for
// a/b and beneath it, "" for none), checked again every revalidate.
func testGrant(tb testing.TB, publish, subscribe string, revalidate time.Duration) *auth.Grant {
	tb.Helper()
	var scopes []token.Scope
	for _, role := range []struct {
		pattern string
		actions []token.Action
	}{
		{publish, []token.Action{token.ActionPublish}},
		{subscribe, []token.Action{token.ActionSubscribe, token.ActionFetch}},
	} {
		if role.pattern == "" {
			continue
		}
		base, ok := strings.CutSuffix("/"+role.pattern, "/**")
		require.True(tb, ok, "pattern %q is not a subtree", role.pattern)
		scopes = append(scopes, token.Scope{Actions: role.actions, Broadcast: strings.TrimPrefix(base, "/"), Prefix: true})
	}
	return auth.NewGrant(scopes, revalidate)
}
