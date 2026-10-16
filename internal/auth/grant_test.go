package auth

import (
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"

	"github.com/qumo-dev/qumo/token"
)

func TestNewGrant(t *testing.T) {
	tg := token.Grant{Root: "acme/app", Publish: []string{"/"}, Subject: "42"}

	g := NewGrant(tg, 30*time.Second)

	assert.Equal(t, tg, g.grant)
	assert.Equal(t, 30*time.Second, g.Revalidate())
	assert.Equal(t, "42", g.Subject())
	assert.Empty(t, g.KeyID())
}

func TestGrant_Allows(t *testing.T) {
	room := NewGrant(token.Grant{
		Root:      "room/123",
		Publish:   []string{"/comments"},
		Subscribe: []string{"/"},
	}, 0)
	everything := NewGrant(token.Grant{Subscribe: []string{"/"}}, 0)
	tests := map[string]struct {
		grant  *Grant
		action token.Action
		path   moqt.BroadcastPath
		want   bool
	}{
		"the exact path":                    {grant: room, action: token.ActionPublish, path: "/room/123/comments", want: true},
		"beneath an exact path":             {grant: room, action: token.ActionPublish, path: "/room/123/comments/x"},
		"beneath the root":                  {grant: room, action: token.ActionSubscribe, path: "/room/123/x", want: true},
		"a sibling of the root":             {grant: room, action: token.ActionSubscribe, path: "/room/1234"},
		"an action not granted":             {grant: room, action: token.ActionCreate, path: "/room/123/comments"},
		"an unknown action":                 {grant: room, action: "delete", path: "/room/123/x"},
		"everything: any path":              {grant: everything, action: token.ActionSubscribe, path: "/any/path", want: true},
		"everything: the root":              {grant: everything, action: token.ActionSubscribe, path: "/", want: true},
		"everything: an action not granted": {grant: everything, action: token.ActionPublish, path: "/any"},
		"nothing granted":                   {grant: &Grant{}, action: token.ActionSubscribe, path: "/room"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.grant.Allows(tt.action, tt.path))
		})
	}
}

// A relay session's MoQ grant is its token's root and publish and subscribe
// patterns, unchanged.
func TestGrant_MoQ(t *testing.T) {
	tests := map[string]struct {
		grant token.Grant
		want  moqt.Grant
	}{
		"a voice room: subscribe beneath the room, publish one broadcast": {
			grant: token.Grant{Root: "room/42", Publish: []string{"/7"}, Subscribe: []string{"/"}},
			want:  moqt.Grant{Root: "/room/42/", Publish: []string{"/7"}, Subscribe: []string{"/"}},
		},
		"the empty root": {
			grant: token.Grant{Publish: []string{"/live"}},
			want:  moqt.Grant{Root: "/", Publish: []string{"/live"}},
		},
		"a funnel's actions alone grant nothing": {
			grant: token.Grant{Root: "room/42", Create: []string{"/comments"}, Post: []string{"/comments"}, Redact: []string{"/"}},
			want:  moqt.Grant{},
		},
		"no patterns": {grant: token.Grant{Root: "room/42"}, want: moqt.Grant{}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, NewGrant(tt.grant, 0).MoQ())
		})
	}
}
