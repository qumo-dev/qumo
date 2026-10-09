package auth

import (
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"

	"github.com/qumo-dev/qumo/token"
)

// publishPrefix is a scope publishing every track at or beneath base.
func publishPrefix(base string) token.Scope {
	return token.Scope{Actions: []token.Action{token.ActionPublish}, Broadcast: base, Prefix: true}
}

// readEverything is a scope subscribing to and fetching every broadcast.
var readEverything = token.Scope{Actions: []token.Action{token.ActionSubscribe, token.ActionFetch}, Prefix: true}

func TestNewGrant(t *testing.T) {
	scopes := []token.Scope{publishPrefix("acme/app"), readEverything}

	g := NewGrant(scopes, 30*time.Second)

	assert.Equal(t, scopes, g.scopes)
	assert.Equal(t, 30*time.Second, g.Revalidate())
	assert.Empty(t, g.Subject())
}

func TestGrant_Allows(t *testing.T) {
	paths := NewGrant([]token.Scope{
		publishPrefix("room/123"),
		{Actions: []token.Action{token.ActionSubscribe, token.ActionFetch}, Broadcast: "room", Prefix: true},
	}, 0)
	everything := NewGrant([]token.Scope{readEverything}, 0)
	scopedGrant := &Grant{subject: "42", scopes: []token.Scope{
		{Actions: []token.Action{token.ActionPublish}, Broadcast: "room/123/comments", Track: "chat"},
		{Actions: []token.Action{token.ActionSubscribe}, Broadcast: "room/123", Prefix: true},
	}}
	tests := map[string]struct {
		grant  *Grant
		action token.Action
		path   moqt.BroadcastPath
		track  moqt.TrackName
		want   bool
	}{
		"paths: publish within":             {grant: paths, action: token.ActionPublish, path: "/room/123/x", track: "t", want: true},
		"paths: publish outside":            {grant: paths, action: token.ActionPublish, path: "/room/9", track: "t"},
		"paths: subscribe within":           {grant: paths, action: token.ActionSubscribe, path: "/room/9", track: "t", want: true},
		"paths: fetch within":               {grant: paths, action: token.ActionFetch, path: "/room/9", track: "t", want: true},
		"paths: an unknown action":          {grant: paths, action: "delete", path: "/room/123", track: "t"},
		"everything: any path":              {grant: everything, action: token.ActionSubscribe, path: "/any/path", track: "t", want: true},
		"everything: the root":              {grant: everything, action: token.ActionFetch, path: "/", track: "t", want: true},
		"everything: an action not granted": {grant: everything, action: token.ActionPublish, path: "/any", track: "t"},
		"scopes: the exact track":           {grant: scopedGrant, action: token.ActionPublish, path: "/room/123/comments", track: "chat", want: true},
		"scopes: another track":             {grant: scopedGrant, action: token.ActionPublish, path: "/room/123/comments", track: "other"},
		"scopes: beneath an exact path":     {grant: scopedGrant, action: token.ActionPublish, path: "/room/123/comments/x", track: "chat"},
		"scopes: a second scope":            {grant: scopedGrant, action: token.ActionSubscribe, path: "/room/123/comments", track: "chat", want: true},
		"scopes: under the prefix":          {grant: scopedGrant, action: token.ActionSubscribe, path: "/room/123/x", track: "t", want: true},
		"scopes: a sibling of the prefix":   {grant: scopedGrant, action: token.ActionSubscribe, path: "/room/1234", track: "t"},
		"scopes: an action not granted":     {grant: scopedGrant, action: token.ActionFetch, path: "/room/123/comments", track: "chat"},
		"nothing granted":                   {grant: &Grant{}, action: token.ActionSubscribe, path: "/room", track: "t"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.grant.Allows(tt.action, tt.path, tt.track))
		})
	}
}

func TestGrant_Announces(t *testing.T) {
	paths := NewGrant([]token.Scope{publishPrefix("room/123"), readEverything}, 0)
	chat := &Grant{scopes: []token.Scope{
		{Actions: []token.Action{token.ActionPublish}, Broadcast: "room/123/comments", Track: "chat"},
		{Actions: []token.Action{token.ActionSubscribe, token.ActionFetch}, Broadcast: "room", Prefix: true},
	}}
	comments := &Grant{scopes: []token.Scope{
		{Actions: []token.Action{token.ActionPublish}, Broadcast: "room/123/comments"},
	}}
	tests := map[string]struct {
		grant *Grant
		path  moqt.BroadcastPath
		want  bool
	}{
		"a publish prefix":                  {grant: paths, path: "/room/123/live", want: true},
		"outside the publish prefix":        {grant: paths, path: "/room/9", want: false},
		"a publish scope naming one track":  {grant: chat, path: "/room/123/comments", want: false},
		"an exact publish scope":            {grant: comments, path: "/room/123/comments", want: true},
		"beneath an exact publish scope":    {grant: chat, path: "/room/123/comments/x", want: false},
		"a subscribe and fetch scope only":  {grant: chat, path: "/room/9", want: false},
		"another spelling of the broadcast": {grant: chat, path: "/room/123/comments/", want: false},
		"nothing granted":                   {grant: &Grant{}, path: "/room", want: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.grant.Announces(tt.path))
		})
	}
}
