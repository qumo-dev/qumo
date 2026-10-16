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
		{Actions: []token.Action{token.ActionPublish}, Broadcast: "room/123/comments"},
		{Actions: []token.Action{token.ActionSubscribe}, Broadcast: "room/123", Prefix: true},
	}}
	tests := map[string]struct {
		grant  *Grant
		action token.Action
		path   moqt.BroadcastPath
		want   bool
	}{
		"paths: publish within":             {grant: paths, action: token.ActionPublish, path: "/room/123/x", want: true},
		"paths: publish outside":            {grant: paths, action: token.ActionPublish, path: "/room/9"},
		"paths: subscribe within":           {grant: paths, action: token.ActionSubscribe, path: "/room/9", want: true},
		"paths: fetch within":               {grant: paths, action: token.ActionFetch, path: "/room/9", want: true},
		"paths: an unknown action":          {grant: paths, action: "delete", path: "/room/123"},
		"everything: any path":              {grant: everything, action: token.ActionSubscribe, path: "/any/path", want: true},
		"everything: the root":              {grant: everything, action: token.ActionFetch, path: "/", want: true},
		"everything: an action not granted": {grant: everything, action: token.ActionPublish, path: "/any"},
		"scopes: the exact path":            {grant: scopedGrant, action: token.ActionPublish, path: "/room/123/comments", want: true},
		"scopes: beneath an exact path":     {grant: scopedGrant, action: token.ActionPublish, path: "/room/123/comments/x"},
		"scopes: a second scope":            {grant: scopedGrant, action: token.ActionSubscribe, path: "/room/123/comments", want: true},
		"scopes: under the prefix":          {grant: scopedGrant, action: token.ActionSubscribe, path: "/room/123/x", want: true},
		"scopes: a sibling of the prefix":   {grant: scopedGrant, action: token.ActionSubscribe, path: "/room/1234"},
		"scopes: an action not granted":     {grant: scopedGrant, action: token.ActionFetch, path: "/room/123/comments"},
		"nothing granted":                   {grant: &Grant{}, action: token.ActionSubscribe, path: "/room"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.grant.Allows(tt.action, tt.path))
		})
	}
}

func TestGrant_MoQ(t *testing.T) {
	subscribe := []token.Action{token.ActionSubscribe, token.ActionFetch}
	publish := []token.Action{token.ActionPublish}
	tests := map[string]struct {
		scopes    []token.Scope
		keyPrefix string
		want      moqt.Grant
	}{
		"a voice room: subscribe beneath the room, publish one broadcast": {
			scopes: []token.Scope{
				{Actions: subscribe, Broadcast: "room/42", Prefix: true},
				{Actions: publish, Broadcast: "room/42/7"},
			},
			want: moqt.Grant{Root: "/room/42/", Publish: []string{"/7"}, Subscribe: []string{"/"}},
		},
		"a single exact broadcast": {
			scopes: []token.Scope{{Actions: subscribe, Broadcast: "room/42/7"}},
			want:   moqt.Grant{Root: "/room/42/", Subscribe: []string{"/7"}},
		},
		"a single broadcast at the top": {
			scopes: []token.Scope{{Actions: publish, Broadcast: "live"}},
			want:   moqt.Grant{Root: "/", Publish: []string{"/live"}},
		},
		"a prefix alone": {
			scopes: []token.Scope{{Actions: publish, Broadcast: "room/42", Prefix: true}},
			want:   moqt.Grant{Root: "/room/42/", Publish: []string{"/"}},
		},
		"publish and subscribe in one scope": {
			scopes: []token.Scope{{Actions: []token.Action{token.ActionPublish, token.ActionSubscribe}, Broadcast: "room/42", Prefix: true}},
			want:   moqt.Grant{Root: "/room/42/", Publish: []string{"/"}, Subscribe: []string{"/"}},
		},
		"disjoint trees share only the root": {
			scopes: []token.Scope{
				{Actions: publish, Broadcast: "a/b", Prefix: true},
				{Actions: subscribe, Broadcast: "c/d"},
			},
			want: moqt.Grant{Root: "/", Publish: []string{"/a/b/"}, Subscribe: []string{"/c/d"}},
		},
		"siblings sharing a string prefix share their parent": {
			scopes: []token.Scope{
				{Actions: subscribe, Broadcast: "room/4", Prefix: true},
				{Actions: subscribe, Broadcast: "room/42", Prefix: true},
			},
			want: moqt.Grant{Root: "/room/", Subscribe: []string{"/4/", "/42/"}},
		},
		"a path and everything beneath it": {
			scopes: token.PathAndBeneath("room/42", token.ActionSubscribe, token.ActionFetch),
			want:   moqt.Grant{Root: "/room/", Subscribe: []string{"/42", "/42/"}},
		},
		"duplicates are granted once": {
			scopes: []token.Scope{
				{Actions: subscribe, Broadcast: "room/42", Prefix: true},
				{Actions: []token.Action{token.ActionSubscribe}, Broadcast: "room/42", Prefix: true},
				{Actions: publish, Broadcast: "room/42/7"},
				{Actions: publish, Broadcast: "room/42/7"},
			},
			want: moqt.Grant{Root: "/room/42/", Publish: []string{"/7"}, Subscribe: []string{"/"}},
		},
		"a funnel's actions alone grant nothing": {
			scopes: []token.Scope{
				{Actions: []token.Action{token.ActionCreate, token.ActionPost, token.ActionRedact}, Broadcast: "room/42/comments"},
				{Actions: []token.Action{token.ActionFetch}, Broadcast: "room", Prefix: true},
			},
			want: moqt.Grant{},
		},
		"a funnel's scope doesn't widen the root": {
			scopes: []token.Scope{
				{Actions: []token.Action{token.ActionFetch}, Broadcast: "other", Prefix: true},
				{Actions: publish, Broadcast: "room/42/7"},
			},
			want: moqt.Grant{Root: "/room/42/", Publish: []string{"/7"}},
		},
		"every broadcast": {
			scopes: []token.Scope{readEverything},
			want:   moqt.Grant{Root: "/", Subscribe: []string{"/"}},
		},
		"no scopes": {want: moqt.Grant{}},
		// The root never lies above the signing key's prefix, so a session
		// lists nothing of another tenant.
		"beneath the key's prefix": {
			scopes:    []token.Scope{{Actions: subscribe, Broadcast: "acme/room/42", Prefix: true}, {Actions: publish, Broadcast: "acme/live"}},
			keyPrefix: "acme",
			want:      moqt.Grant{Root: "/acme/", Publish: []string{"/live"}, Subscribe: []string{"/room/42/"}},
		},
		"the key's prefix and beneath it": {
			scopes:    token.PathAndBeneath("acme", token.ActionSubscribe, token.ActionFetch),
			keyPrefix: "acme",
			want:      moqt.Grant{Root: "/acme/", Subscribe: []string{"/"}},
		},
		"an exact scope on the key's prefix is left out": {
			scopes:    []token.Scope{{Actions: publish, Broadcast: "acme"}},
			keyPrefix: "acme",
			want:      moqt.Grant{},
		},
		"a deep key prefix": {
			scopes:    token.PathAndBeneath("acme/app", token.ActionPublish),
			keyPrefix: "acme/app",
			want:      moqt.Grant{Root: "/acme/app/", Publish: []string{"/"}},
		},
		"the same exact scope without a key prefix": {
			scopes: []token.Scope{{Actions: publish, Broadcast: "acme"}},
			want:   moqt.Grant{Root: "/", Publish: []string{"/acme"}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			g := &Grant{scopes: tt.scopes, keyPrefix: tt.keyPrefix}

			assert.Equal(t, tt.want, g.MoQ())
		})
	}
}

func TestCommonDir(t *testing.T) {
	tests := map[string]struct {
		a, b string
		want string
	}{
		"the same directory":               {a: "/room/42/", b: "/room/42/", want: "/room/42/"},
		"b beneath a":                      {a: "/room/", b: "/room/42/", want: "/room/"},
		"a beneath b":                      {a: "/room/42/", b: "/room/", want: "/room/"},
		"siblings":                         {a: "/room/42/", b: "/room/43/", want: "/room/"},
		"siblings sharing a string prefix": {a: "/room/4/", b: "/room/42/", want: "/room/"},
		"disjoint":                         {a: "/a/b/", b: "/c/d/", want: "/"},
		"the root":                         {a: "/", b: "/c/", want: "/"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, commonDir(tt.a, tt.b))
		})
	}
}
