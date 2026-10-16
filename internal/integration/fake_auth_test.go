//go:build integration

package integration

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
// relay.Server.Authorize takes: err when set, otherwise grant. The zero value
// admits every session with a grant that covers nothing. It records every
// request.
// revalidateErr, when set, answers every revalidate instead.
type fakeAuth struct {
	grant         *auth.Grant
	err           error
	revalidateErr error
	// hold, when set, keeps every answer back until it is closed: a
	// session's admission stays pending for as long.
	hold chan struct{}

	mu       sync.Mutex
	requests []auth.Request
	ends     []auth.Request
}

// end records an end report, which is what relay.Server.End takes.
func (f *fakeAuth) end(_ context.Context, req auth.Request) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ends = append(f.ends, req)
	return nil
}

// ended returns a copy of the end reports seen so far.
func (f *fakeAuth) ended() []auth.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]auth.Request(nil), f.ends...)
}

func (f *fakeAuth) authorize(ctx context.Context, req auth.Request) (*auth.Grant, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.hold != nil {
		select {
		case <-f.hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if req.Event == auth.EventRevalidate && f.revalidateErr != nil {
		return nil, f.revalidateErr
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.grant == nil {
		return &auth.Grant{}, nil
	}
	return f.grant, nil
}

// testGrant returns a grant that may publish beneath one subtree pattern and
// subscribe beneath another ("**" for everything, "a/b/**" for
// a/b and beneath it, "" for none), checked again every revalidate. Its root
// is the deepest directory both subtrees lie in.
func testGrant(tb testing.TB, publish, subscribe string, revalidate time.Duration) *auth.Grant {
	tb.Helper()
	var g token.Grant
	roles := []struct {
		pattern  string
		patterns *[]string
		base     string
	}{
		{pattern: publish, patterns: &g.Publish},
		{pattern: subscribe, patterns: &g.Subscribe},
	}
	var dirs []string
	for i, role := range roles {
		if role.pattern == "" {
			continue
		}
		base, ok := strings.CutSuffix(role.pattern, "**")
		require.True(tb, ok, "pattern %q is not a subtree", role.pattern)
		roles[i].base = strings.TrimSuffix(base, "/")
		dirs = append(dirs, parentDir(roles[i].base))
	}
	for i, d := range dirs {
		if i == 0 {
			g.Root = d
			continue
		}
		for g.Root != "" && d != g.Root && !strings.HasPrefix(d, g.Root+"/") {
			g.Root = parentDir(g.Root)
		}
	}
	for _, role := range roles {
		if role.pattern == "" {
			continue
		}
		patterns := []string{"/"}
		if role.base != "" {
			rel := "/" + strings.TrimPrefix(strings.TrimPrefix(role.base, g.Root), "/")
			patterns = []string{rel, rel + "/"}
		}
		*role.patterns = patterns
	}
	return auth.NewGrant(g, revalidate)
}

// parentDir is the directory path lies in: "a" for "a/b", "" for "a".
func parentDir(path string) string {
	return path[:max(strings.LastIndex(path, "/"), 0)]
}

// received returns a copy of the requests seen so far.
func (f *fakeAuth) received() []auth.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]auth.Request(nil), f.requests...)
}

// authOff admits every session unchecked, as a relay with auth off does
// (QUMO_AUTH_KEYS unset): a nil grant with a nil error.
func authOff(context.Context, auth.Request) (*auth.Grant, error) {
	return nil, nil
}

// nativeURL is the native-QUIC (moqt://) URL of a relay at addr.
func nativeURL(addr string) string {
	return "moqt://" + addr
}

// exactly grants actions on the broadcast at path, "a/b", alone: the pattern
// "/b" beneath the root "a".
func exactly(path string, actions ...token.Action) token.Grant {
	return grantOn(parentDir(path), []string{"/" + path[strings.LastIndex(path, "/")+1:]}, actions...)
}

// atAndBeneath grants actions on the broadcast at path, "a/b", and on every
// broadcast beneath it: the patterns "/b" and "/b/" beneath the root "a".
func atAndBeneath(path string, actions ...token.Action) token.Grant {
	base := "/" + path[strings.LastIndex(path, "/")+1:]
	return grantOn(parentDir(path), []string{base, base + "/"}, actions...)
}

// grantOn grants actions on patterns beneath root.
func grantOn(root string, patterns []string, actions ...token.Action) token.Grant {
	g := token.Grant{Root: root}
	for _, a := range actions {
		switch a {
		case token.ActionPublish:
			g.Publish = patterns
		case token.ActionSubscribe:
			g.Subscribe = patterns
		case token.ActionCreate:
			g.Create = patterns
		case token.ActionPost:
			g.Post = patterns
		case token.ActionRedact:
			g.Redact = patterns
		}
	}
	return g
}
