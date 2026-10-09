package token

import (
	"encoding/base64"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scopedClaims are the claims of a current token whose grant is scopes.
func scopedClaims(now time.Time, scopes any) map[string]any {
	c := validClaims(now)
	delete(c, "path_auth")
	c["scopes"] = scopes
	return c
}

func TestVerify_Scopes(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	publishChat := map[string]any{
		"actions":   []string{"publish"},
		"broadcast": map[string]any{"exact": "acme/app/room/123/comments"},
		"track":     map[string]any{"exact": "chat"},
	}
	tests := map[string]struct {
		prefix  string
		edit    func(c map[string]any)
		want    Grant
		wantErr error
		reason  string
	}{
		"an exact broadcast and track, with a subject": {
			prefix: "acme/app",
			edit:   func(c map[string]any) { c["sub"] = "42" },
			want: Grant{Subject: "42", Scopes: []Scope{
				{Actions: []Action{ActionPublish}, Broadcast: "acme/app/room/123/comments", Track: "chat"},
			}},
		},
		"no subject": {
			want: Grant{Scopes: []Scope{
				{Actions: []Action{ActionPublish}, Broadcast: "acme/app/room/123/comments", Track: "chat"},
			}},
		},
		"a prefix, every track, normalized": {
			edit: func(c map[string]any) {
				c["scopes"] = []any{map[string]any{
					"actions":   []string{"subscribe", "fetch"},
					"broadcast": map[string]any{"prefix": "/acme//app/room/"},
				}}
			},
			want: Grant{Scopes: []Scope{
				{Actions: []Action{ActionSubscribe, ActionFetch}, Broadcast: "acme/app/room", Prefix: true},
			}},
		},
		"exactly the key's prefix": {
			prefix: "acme/app",
			edit: func(c map[string]any) {
				c["scopes"] = []any{map[string]any{"actions": []string{"fetch"}, "broadcast": map[string]any{"prefix": "acme/app"}}}
			},
			want: Grant{Scopes: []Scope{{Actions: []Action{ActionFetch}, Broadcast: "acme/app", Prefix: true}}},
		},
		"another tenant's broadcast": {
			prefix:  "acme/app",
			edit:    func(c map[string]any) { c["scopes"] = []any{scopeOn("other/app/room", "exact")} },
			wantErr: ErrForbidden, reason: "prefix",
		},
		"a sibling sharing a string prefix": {
			prefix:  "acme/app",
			edit:    func(c map[string]any) { c["scopes"] = []any{scopeOn("acme/apple", "prefix")} },
			wantErr: ErrForbidden, reason: "prefix",
		},
		"a parent of the key's prefix": {
			prefix:  "acme/app",
			edit:    func(c map[string]any) { c["scopes"] = []any{scopeOn("acme", "prefix")} },
			wantErr: ErrForbidden, reason: "prefix",
		},
		"one scope of two escapes the prefix": {
			prefix:  "acme/app",
			edit:    func(c map[string]any) { c["scopes"] = []any{publishChat, scopeOn("other", "exact")} },
			wantErr: ErrForbidden, reason: "scope 1",
		},
		"empty scopes":  {edit: func(c map[string]any) { c["scopes"] = []any{} }, wantErr: ErrForbidden, reason: "nothing"},
		"null scopes":   {edit: func(c map[string]any) { c["scopes"] = nil }, wantErr: ErrForbidden, reason: "nothing"},
		"both claims":   {edit: func(c map[string]any) { c["path_auth"] = map[string]any{"root": "acme", "pub": ""} }, wantErr: ErrInvalid, reason: "not both"},
		"neither claim": {edit: func(c map[string]any) { delete(c, "scopes") }, wantErr: ErrInvalid, reason: "required"},
		"scopes is not a list": {
			edit:    func(c map[string]any) { c["scopes"] = publishChat },
			wantErr: ErrInvalid, reason: "scopes",
		},
		"an unknown action": {
			edit: func(c map[string]any) {
				c["scopes"] = []any{withMember(publishChat, "actions", []string{"publish", "delete"})}
			},
			wantErr: ErrInvalid, reason: `"delete"`,
		},
		"announce, not an action": {
			edit:    func(c map[string]any) { c["scopes"] = []any{withMember(publishChat, "actions", []string{"announce"})} },
			wantErr: ErrInvalid, reason: `"announce"`,
		},
		"an action in another case": {
			edit:    func(c map[string]any) { c["scopes"] = []any{withMember(publishChat, "actions", []string{"PUBLISH"})} },
			wantErr: ErrInvalid, reason: `"PUBLISH"`,
		},
		"no actions": {
			edit:    func(c map[string]any) { c["scopes"] = []any{withMember(publishChat, "actions", []string{})} },
			wantErr: ErrInvalid, reason: "no actions",
		},
		"no broadcast match": {
			edit:    func(c map[string]any) { c["scopes"] = []any{withMember(publishChat, "broadcast", nil)} },
			wantErr: ErrInvalid, reason: "broadcast",
		},
		"an empty broadcast match": {
			edit:    func(c map[string]any) { c["scopes"] = []any{withMember(publishChat, "broadcast", map[string]any{})} },
			wantErr: ErrInvalid, reason: "exactly one",
		},
		"both exact and prefix": {
			edit: func(c map[string]any) {
				c["scopes"] = []any{withMember(publishChat, "broadcast", map[string]any{"exact": "a", "prefix": "a"})}
			},
			wantErr: ErrInvalid, reason: "exactly one",
		},
		"a suffix match": {
			edit: func(c map[string]any) {
				c["scopes"] = []any{withMember(publishChat, "broadcast", map[string]any{"suffix": "chat"})}
			},
			wantErr: ErrInvalid, reason: "suffix",
		},
		"a broadcast naming no path": {
			edit:    func(c map[string]any) { c["scopes"] = []any{scopeOn("/", "prefix")} },
			wantErr: ErrInvalid, reason: "no path",
		},
		"dot-dot in the broadcast": {
			edit:    func(c map[string]any) { c["scopes"] = []any{scopeOn("acme/app/../other", "exact")} },
			wantErr: ErrInvalid, reason: "..",
		},
		"a wildcard broadcast": {
			edit:    func(c map[string]any) { c["scopes"] = []any{scopeOn("acme/*", "prefix")} },
			wantErr: ErrInvalid, reason: "*",
		},
		"a track prefix": {
			edit: func(c map[string]any) {
				c["scopes"] = []any{withMember(publishChat, "track", map[string]any{"prefix": "ch"})}
			},
			wantErr: ErrInvalid, reason: "prefix",
		},
		"a misspelled track match": {
			edit: func(c map[string]any) {
				c["scopes"] = []any{withMember(publishChat, "tracks", map[string]any{"exact": "chat"})}
			},
			wantErr: ErrInvalid, reason: "tracks",
		},
		"an empty track match": {
			edit: func(c map[string]any) {
				c["scopes"] = []any{withMember(publishChat, "track", map[string]any{"exact": ""})}
			},
			wantErr: ErrInvalid, reason: "track",
		},
		"sub is not a string": {
			edit:    func(c map[string]any) { c["sub"] = 42 },
			wantErr: ErrInvalid, reason: "claims",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			signer := &rawSigner{prefix: tt.prefix}
			c := scopedClaims(now, []any{publishChat})
			if tt.edit != nil {
				tt.edit(c)
			}

			got, err := Verify(signer.sign(t, c), signer.keys(t), now)

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.Contains(t, err.Error(), tt.reason)
				assert.Equal(t, Claims{}, got, "a refused token grants nothing")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.Grant)
		})
	}
}

// scopeOn is a publish scope on broadcast, matched by match ("exact" or
// "prefix").
func scopeOn(broadcast, match string) map[string]any {
	return map[string]any{"actions": []string{"publish"}, "broadcast": map[string]any{match: broadcast}}
}

// withMember returns a copy of scope with member set to v, or removed for nil.
func withMember(scope map[string]any, member string, v any) map[string]any {
	out := map[string]any{}
	for k, val := range scope {
		out[k] = val
	}
	if v == nil {
		delete(out, member)
		return out
	}
	out[member] = v
	return out
}

func TestSign_ScopesRoundTrip(t *testing.T) {
	key, err := GenerateKey("acme/app")
	require.NoError(t, err)
	now := time.Unix(1_800_000_000, 0)
	g := Grant{Subject: "42", Scopes: []Scope{
		{Actions: []Action{ActionPost}, Broadcast: "/acme/app/room/123/comments/", Track: "chat"},
		{Actions: []Action{ActionPublish}, Broadcast: "acme/app/room/123/42"},
		{Actions: []Action{ActionSubscribe, ActionFetch}, Broadcast: "acme/app/room/123", Prefix: true},
	}}

	tok, err := signAt(key, g, 30*time.Minute, now)
	require.NoError(t, err)
	c, err := Verify(tok, map[string]Key{key.ID: key.Public()}, now)

	require.NoError(t, err)
	assert.Equal(t, Grant{Subject: "42", Scopes: []Scope{
		{Actions: []Action{ActionPost}, Broadcast: "acme/app/room/123/comments", Track: "chat"},
		{Actions: []Action{ActionPublish}, Broadcast: "acme/app/room/123/42"},
		{Actions: []Action{ActionSubscribe, ActionFetch}, Broadcast: "acme/app/room/123", Prefix: true},
	}}, c.Grant)
}

func TestSign_ScopesClaimShape(t *testing.T) {
	key, err := GenerateKey("")
	require.NoError(t, err)
	g := Grant{Subject: "42", Scopes: []Scope{
		{Actions: []Action{ActionPublish}, Broadcast: "room/123/comments", Track: "chat"},
	}}

	tok, err := Sign(key, g, time.Minute)
	require.NoError(t, err)
	raw, err := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[1])
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, json.Unmarshal(raw, &claims))

	assert.Equal(t, "42", claims["sub"])
	assert.Equal(t, []any{map[string]any{
		"actions":   []any{"publish"},
		"broadcast": map[string]any{"exact": "room/123/comments"},
		"track":     map[string]any{"exact": "chat"},
	}}, claims["scopes"])
	assert.NotContains(t, claims, "path_auth")
}

func TestSign_ScopeRefusals(t *testing.T) {
	chat := Scope{Actions: []Action{ActionPublish}, Broadcast: "acme/app/room", Track: "chat"}
	tests := map[string]struct {
		grant       Grant
		wantErrText string
	}{
		"a subject alone":          {grant: Grant{Subject: "42"}, wantErrText: "no scope"},
		"announce":                 {grant: Grant{Scopes: []Scope{{Actions: []Action{"announce"}, Broadcast: "acme/app/room"}}}, wantErrText: "unknown action"},
		"a prefix of everything":   {grant: Grant{Scopes: []Scope{{Actions: []Action{ActionFetch}, Prefix: true}}}, wantErrText: "segments"},
		"no actions":               {grant: Grant{Scopes: []Scope{{Broadcast: "acme/app/room"}}}, wantErrText: "no actions"},
		"an unknown action":        {grant: Grant{Scopes: []Scope{{Actions: []Action{"delete"}, Broadcast: "acme/app/room"}}}, wantErrText: "unknown action"},
		"no broadcast":             {grant: Grant{Scopes: []Scope{{Actions: []Action{ActionFetch}}}}, wantErrText: "segments"},
		"a wildcard broadcast":     {grant: Grant{Scopes: []Scope{{Actions: []Action{ActionFetch}, Broadcast: "acme/app/*"}}}, wantErrText: "segments"},
		"outside the key's prefix": {grant: Grant{Scopes: []Scope{{Actions: []Action{ActionFetch}, Broadcast: "acme/apple"}}}, wantErrText: "outside"},
		"a later scope escapes":    {grant: Grant{Scopes: []Scope{chat, {Actions: []Action{ActionFetch}, Broadcast: "other"}}}, wantErrText: "scope 1"},
		"a prefix above the key's": {grant: Grant{Scopes: []Scope{{Actions: []Action{ActionFetch}, Broadcast: "acme", Prefix: true}}}, wantErrText: "outside"},
		"a dot-dot escape":         {grant: Grant{Scopes: []Scope{{Actions: []Action{ActionFetch}, Broadcast: "acme/app/../x"}}}, wantErrText: "segments"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			key, err := GenerateKey("acme/app")
			require.NoError(t, err)

			_, err = Sign(key, tt.grant, time.Minute)

			assert.ErrorContains(t, err, tt.wantErrText)
		})
	}
}

func TestScope_Allows(t *testing.T) {
	exact := Scope{Actions: []Action{ActionPublish}, Broadcast: "room/123/comments", Track: "chat"}
	prefix := Scope{Actions: []Action{ActionSubscribe, ActionFetch}, Broadcast: "room/123", Prefix: true}
	anyTrack := Scope{Actions: []Action{ActionSubscribe}, Broadcast: "room/123/comments"}
	everything := Scope{Actions: []Action{ActionFetch}, Prefix: true}
	tests := map[string]struct {
		scope     Scope
		action    Action
		broadcast string
		track     string
		want      bool
	}{
		"the exact broadcast and track":     {scope: exact, action: ActionPublish, broadcast: "/room/123/comments", track: "chat", want: true},
		"another track":                     {scope: exact, action: ActionPublish, broadcast: "/room/123/comments", track: "other"},
		"no track for a track scope":        {scope: exact, action: ActionPublish, broadcast: "/room/123/comments"},
		"an action not listed":              {scope: exact, action: ActionFetch, broadcast: "/room/123/comments", track: "chat"},
		"an unknown action":                 {scope: exact, action: "delete", broadcast: "/room/123/comments", track: "chat"},
		"beneath an exact broadcast":        {scope: exact, action: ActionPublish, broadcast: "/room/123/comments/user-42", track: "chat"},
		"above an exact broadcast":          {scope: exact, action: ActionPublish, broadcast: "/room/123", track: "chat"},
		"the prefix itself":                 {scope: prefix, action: ActionFetch, broadcast: "/room/123", track: "x", want: true},
		"beneath the prefix":                {scope: prefix, action: ActionSubscribe, broadcast: "/room/123/comments", track: "x", want: true},
		"a sibling sharing a string prefix": {scope: prefix, action: ActionSubscribe, broadcast: "/room/1234", track: "x"},
		"above the prefix":                  {scope: prefix, action: ActionSubscribe, broadcast: "/room", track: "x"},
		"any track":                         {scope: anyTrack, action: ActionSubscribe, broadcast: "/room/123/comments", track: "x", want: true},
		"another spelling of the broadcast": {scope: exact, action: ActionPublish, broadcast: "/room//123/comments/", track: "chat"},
		"another spelling beneath a prefix": {scope: prefix, action: ActionFetch, broadcast: "/room/123//comments", track: "x"},
		"without the leading slash":         {scope: exact, action: ActionPublish, broadcast: "room/123/comments", track: "chat", want: true},
		"a dot-dot broadcast":               {scope: prefix, action: ActionFetch, broadcast: "/room/123/../9", track: "x"},
		"the zero scope":                    {scope: Scope{Prefix: true}, action: ActionFetch, broadcast: "/room", track: "x"},
		"a prefix of everything":            {scope: everything, action: ActionFetch, broadcast: "/room/123", track: "x", want: true},
		"a prefix of everything, the root":  {scope: everything, action: ActionFetch, broadcast: "/", track: "x", want: true},
		"a prefix of everything, unlisted":  {scope: everything, action: ActionPublish, broadcast: "/room", track: "x"},
		"a prefix of everything, respelled": {scope: everything, action: ActionFetch, broadcast: "/room//123", track: "x"},
		"an exact scope with no broadcast":  {scope: Scope{Actions: []Action{ActionFetch}}, action: ActionFetch, broadcast: "/", track: "x"},
		"the empty path":                    {scope: prefix, action: ActionFetch, broadcast: "", track: "x"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.scope.Allows(tt.action, tt.broadcast, tt.track))
		})
	}
}
