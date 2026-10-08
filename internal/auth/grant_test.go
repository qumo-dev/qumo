package auth

import (
	"fmt"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/token"
)

func TestPattern_Contains(t *testing.T) {
	tests := map[string]struct {
		pattern string
		path    string
		want    bool
	}{
		"everything":                      {pattern: "**", path: "/any/where", want: true},
		"everything contains the root":    {pattern: "**", path: "/", want: true},
		"the base itself":                 {pattern: "acme/app/**", path: "/acme/app", want: true},
		"beneath the base":                {pattern: "acme/app/**", path: "/acme/app/room/1", want: true},
		"sibling sharing a string prefix": {pattern: "acme/app/**", path: "/acme/apple", want: false},
		"parent of the base":              {pattern: "acme/app/**", path: "/acme", want: false},
		"another tenant":                  {pattern: "acme/app/**", path: "/other/app", want: false},
		"case differs":                    {pattern: "acme/app/**", path: "/ACME/app", want: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p, err := parsePattern(tt.pattern)
			require.NoError(t, err)

			got := p.contains(moqt.BroadcastPath(tt.path))

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParsePattern_RefusesUnsupported(t *testing.T) {
	for _, raw := range []string{
		"",          // empty
		"acme",      // an exact path: not a subtree
		"acme/*",    // a segment wildcard
		"*/chat/**", // a wildcard inside the base
		"/acme/**",  // a leading slash makes an empty segment
		"acme//app/**",
		"acme/../other/**",
		"acme/./app/**",
	} {
		t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
			_, err := parsePattern(raw)

			assert.Error(t, err)
		})
	}
}

func TestNewGrant(t *testing.T) {
	expires := time.Unix(1_000_600, 0)

	g, err := NewGrant([]string{"acme/app/**"}, []string{"**"}, expires, 30*time.Second)

	require.NoError(t, err)
	assert.True(t, g.Publish.Contains(moqt.BroadcastPath("/acme/app/live")))
	assert.False(t, g.Publish.Contains(moqt.BroadcastPath("/acme/other")))
	assert.True(t, g.Subscribe.Contains(moqt.BroadcastPath("/anything")))
	assert.Equal(t, expires, g.Expires())
	assert.Equal(t, 30*time.Second, g.Revalidate())
}

func TestPatterns_Bases(t *testing.T) {
	g, err := NewGrant([]string{"acme/app/**", "**"}, nil, time.Time{}, 0)
	require.NoError(t, err)

	assert.Equal(t, []string{"acme/app", ""}, g.Publish.Bases())
	assert.Empty(t, g.Subscribe.Bases())
}

func TestNewGrant_Empty(t *testing.T) {
	g, err := NewGrant(nil, nil, time.Time{}, 0)

	require.NoError(t, err)
	assert.False(t, g.Publish.Contains(moqt.BroadcastPath("/x")))
	assert.False(t, g.Subscribe.Contains(moqt.BroadcastPath("/x")))
	assert.True(t, g.Expires().IsZero())
}

func TestNewGrant_RefusesABadPattern(t *testing.T) {
	tests := map[string]struct {
		publish, subscribe []string
		wantErrText        string
	}{
		"publish":   {publish: []string{"acme/live"}, wantErrText: "publish"},
		"subscribe": {subscribe: []string{"acme/*"}, wantErrText: "subscribe"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NewGrant(tt.publish, tt.subscribe, time.Time{}, 0)

			assert.ErrorContains(t, err, tt.wantErrText)
		})
	}
}

func TestGrant_Allows(t *testing.T) {
	paths, err := NewGrant([]string{"room/123/**"}, []string{"room/**"}, time.Time{}, 0)
	require.NoError(t, err)
	scopedGrant := &Grant{subject: "42", scopes: []token.Scope{
		{Actions: []token.Action{token.ActionPublish}, Broadcast: "room/123/comments", Track: "chat"},
		{Actions: []token.Action{token.ActionAnnounce, token.ActionSubscribe}, Broadcast: "room/123", Prefix: true},
	}}
	tests := map[string]struct {
		grant  *Grant
		action token.Action
		path   moqt.BroadcastPath
		track  moqt.TrackName
		want   bool
	}{
		"paths: publish within":             {grant: paths, action: token.ActionPublish, path: "/room/123/x", track: "t", want: true},
		"paths: announce within":            {grant: paths, action: token.ActionAnnounce, path: "/room/123", want: true},
		"paths: publish outside":            {grant: paths, action: token.ActionPublish, path: "/room/9", track: "t"},
		"paths: subscribe within":           {grant: paths, action: token.ActionSubscribe, path: "/room/9", track: "t", want: true},
		"paths: fetch within":               {grant: paths, action: token.ActionFetch, path: "/room/9", track: "t", want: true},
		"paths: an unknown action":          {grant: paths, action: "delete", path: "/room/123", track: "t"},
		"scopes: the exact track":           {grant: scopedGrant, action: token.ActionPublish, path: "/room/123/comments", track: "chat", want: true},
		"scopes: another track":             {grant: scopedGrant, action: token.ActionPublish, path: "/room/123/comments", track: "other"},
		"scopes: beneath an exact path":     {grant: scopedGrant, action: token.ActionPublish, path: "/room/123/comments/x", track: "chat"},
		"scopes: a second scope":            {grant: scopedGrant, action: token.ActionSubscribe, path: "/room/123/comments", track: "chat", want: true},
		"scopes: announce under the prefix": {grant: scopedGrant, action: token.ActionAnnounce, path: "/room/123/x", want: true},
		"scopes: a sibling of the prefix":   {grant: scopedGrant, action: token.ActionAnnounce, path: "/room/1234"},
		"scopes: an action not granted":     {grant: scopedGrant, action: token.ActionFetch, path: "/room/123/comments", track: "chat"},
		"nothing granted":                   {grant: &Grant{}, action: token.ActionSubscribe, path: "/room", track: "t"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.grant.Allows(tt.action, tt.path, tt.track))
		})
	}
}
