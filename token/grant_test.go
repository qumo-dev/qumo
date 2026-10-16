package token

import (
	"encoding/base64"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// grantClaimsOf are the claims of a current token whose grant is the members
// of grant: root, the actions' patterns and sub.
func grantClaimsOf(now time.Time, grant map[string]any) map[string]any {
	c := validClaims(now)
	delete(c, "path_auth")
	for k, v := range grant {
		c[k] = v
	}
	return c
}

func TestVerify_Grant(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tests := map[string]struct {
		prefix  string
		claims  map[string]any
		want    Grant
		wantErr error
		reason  string
	}{
		"every action, with a subject": {
			prefix: "/acme/",
			claims: map[string]any{
				"root": "/acme/room/42/", "sub": "alice",
				"publish": []string{"/7"}, "subscribe": []string{"/"},
				"create": []string{"/comments"}, "post": []string{"/comments"}, "redact": []string{"/comments/"},
			},
			want: Grant{
				Root: "/acme/room/42/", Publish: []string{"/7"}, Subscribe: []string{"/"},
				Subject: "alice",
				Create:  []string{"/comments"}, Post: []string{"/comments"}, Redact: []string{"/comments/"},
			},
		},
		"a wildcard pattern": {
			claims: map[string]any{"root": "/acme/room/", "subscribe": []string{"/{user}/audio"}},
			want:   Grant{Root: "/acme/room/", Subscribe: []string{"/{user}/audio"}},
		},
		"exactly the key's prefix": {
			prefix: "/acme/app/",
			claims: map[string]any{"root": "/acme/app/", "subscribe": []string{"/"}},
			want:   Grant{Root: "/acme/app/", Subscribe: []string{"/"}},
		},
		"the root of every path": {
			claims: map[string]any{"root": "/", "publish": []string{"/live/"}},
			want:   Grant{Root: "/", Publish: []string{"/live/"}},
		},
		// No root and no patterns name the key's tenant alone, as a session's
		// credential does when its grants come later.
		"no grant claims":        {prefix: "/acme/app/", want: Grant{}},
		"a root and no patterns": {prefix: "/acme/app/", claims: map[string]any{"root": "/acme/app/x/"}, want: Grant{Root: "/acme/app/x/"}},
		"empty patterns":         {claims: map[string]any{"root": "/acme/", "publish": []string{}}, want: Grant{Root: "/acme/", Publish: []string{}}},
		"a subject alone":        {claims: map[string]any{"sub": "42"}, want: Grant{Subject: "42"}},
		"another tenant's root":  {prefix: "/acme/app/", claims: map[string]any{"root": "/other/app/"}, wantErr: ErrForbidden, reason: "prefix"},
		"a sibling sharing a string prefix": {
			prefix: "/acme/app/", claims: map[string]any{"root": "/acme/apple/", "subscribe": []string{"/"}},
			wantErr: ErrForbidden, reason: "prefix",
		},
		"a parent of the key's prefix": {
			prefix: "/acme/app/", claims: map[string]any{"root": "/acme/", "subscribe": []string{"/app/"}},
			wantErr: ErrForbidden, reason: "prefix",
		},
		"patterns without a root": {
			claims:  map[string]any{"subscribe": []string{"/acme/app/"}},
			wantErr: ErrInvalid, reason: "without a root",
		},
		"a funnel's patterns without a root": {
			claims:  map[string]any{"post": []string{"/comments"}},
			wantErr: ErrInvalid, reason: "without a root",
		},
		// A root is a MoQ directory, spelled one way.
		"a root without its slashes":  {claims: map[string]any{"root": "acme/room"}, wantErr: ErrInvalid, reason: "directory"},
		"a root without a trailing /": {claims: map[string]any{"root": "/acme/room"}, wantErr: ErrInvalid, reason: "directory"},
		"a doubled slash in the root": {claims: map[string]any{"root": "/acme//room/"}, wantErr: ErrInvalid, reason: "directory"},
		"dot-dot in the root":         {claims: map[string]any{"root": "/acme/app/../other/"}, wantErr: ErrInvalid, reason: ".."},
		"a wildcard in the root":      {claims: map[string]any{"root": "/acme/*/"}, wantErr: ErrInvalid, reason: "*"},
		"a brace in the root":         {claims: map[string]any{"root": "/acme/{room}/"}, wantErr: ErrInvalid, reason: "{"},
		"a pattern with no slash":     {claims: map[string]any{"root": "/acme/", "publish": []string{"7"}}, wantErr: ErrInvalid, reason: "publish"},
		"an empty pattern":            {claims: map[string]any{"root": "/acme/", "subscribe": []string{""}}, wantErr: ErrInvalid, reason: "subscribe"},
		"a malformed wildcard":        {claims: map[string]any{"root": "/acme/", "subscribe": []string{"/{user"}}, wantErr: ErrInvalid, reason: "subscribe"},
		"a wildcard named twice":      {claims: map[string]any{"root": "/acme/", "post": []string{"/{a}/{a}"}}, wantErr: ErrInvalid, reason: "post"},
		"a rest wildcard not last":    {claims: map[string]any{"root": "/acme/", "create": []string{"/{a...}/x"}}, wantErr: ErrInvalid, reason: "create"},
		"dot-dot in a pattern":        {claims: map[string]any{"root": "/acme/app/", "redact": []string{"/../other"}}, wantErr: ErrInvalid, reason: ".."},
		"a dot in a pattern":          {claims: map[string]any{"root": "/acme/app/", "publish": []string{"/./x"}}, wantErr: ErrInvalid, reason: `"."`},
		"patterns are not a list":     {claims: map[string]any{"root": "/acme/", "publish": "/7"}, wantErr: ErrInvalid, reason: "claims"},
		"a pattern is not a string":   {claims: map[string]any{"root": "/acme/", "publish": []any{7}}, wantErr: ErrInvalid, reason: "claims"},
		"root is not a string":        {claims: map[string]any{"root": 7}, wantErr: ErrInvalid, reason: "claims"},
		"sub is not a string":         {claims: map[string]any{"sub": 42}, wantErr: ErrInvalid, reason: "claims"},
		// A claim it doesn't know is refused rather than ignored, since it
		// might be meant to narrow the grant.
		"the scopes claim": {
			claims:  map[string]any{"scopes": []any{map[string]any{"actions": []string{"publish"}, "broadcast": map[string]any{"exact": "a"}}}},
			wantErr: ErrInvalid, reason: `"scopes"`,
		},
		"an unknown action": {claims: map[string]any{"root": "/acme/", "delete": []string{"/"}}, wantErr: ErrInvalid, reason: `"delete"`},
		// Reading history is subscribe's, so fetch is no action.
		"fetch":    {claims: map[string]any{"root": "/acme/", "fetch": []string{"/"}}, wantErr: ErrInvalid, reason: `"fetch"`},
		"announce": {claims: map[string]any{"root": "/acme/", "announce": []string{"/"}}, wantErr: ErrInvalid, reason: `"announce"`},
		// A token's expiry is its exp alone.
		"a max age": {claims: map[string]any{"root": "/acme/", "max_age": 60}, wantErr: ErrInvalid, reason: `"max_age"`},
		"an action in another case": {
			claims:  map[string]any{"root": "/acme/", "Publish": []string{"/"}},
			wantErr: ErrInvalid, reason: `"Publish"`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			signer := &rawSigner{prefix: tt.prefix}

			got, err := Verify(signer.sign(t, grantClaimsOf(now, tt.claims)), signer.keys(t), now)

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

func TestSign_RoundTrip(t *testing.T) {
	const root = "/acme/app/room/42/"
	tests := map[string]Grant{
		"publish":   {Root: root, Publish: []string{"/7"}},
		"subscribe": {Root: root, Subscribe: []string{"/"}},
		"create":    {Root: root, Create: []string{"/comments"}},
		"post":      {Root: root, Post: []string{"/comments"}},
		"redact":    {Root: root, Redact: []string{"/{room}/comments"}},
		"every action, with a subject": {
			Root: root, Publish: []string{"/7"}, Subscribe: []string{"/"},
			Subject: "alice",
			Create:  []string{"/comments"}, Post: []string{"/comments"}, Redact: []string{"/comments"},
		},
		// A grant of no patterns, with or without a subject, names the key's
		// tenant alone.
		"nothing":         {},
		"a subject alone": {Subject: "42"},
		"a root alone":    {Root: "/acme/app/x/"},
	}
	for name, grant := range tests {
		t.Run(name, func(t *testing.T) {
			key, err := GenerateKey("/acme/app/")
			require.NoError(t, err)
			now := time.Unix(1_800_000_000, 0)

			tok, err := signAt(key, grant, 30*time.Minute, now)
			require.NoError(t, err)
			c, err := Verify(tok, map[string]Key{key.ID: key.Public()}, now)

			require.NoError(t, err)
			assert.Equal(t, grant, c.Grant)
			assert.NoError(t, c.Validate(), "a relay can apply its MoQ grant as it is")
			assert.Equal(t, key.ID, c.Key.ID)
			assert.Equal(t, now.Add(30*time.Minute), c.ExpiresAt)
			assert.Len(t, c.ID, 26, "a random jti (rand.Text)")
		})
	}
}

func TestSign_ClaimShape(t *testing.T) {
	key, err := GenerateKey("")
	require.NoError(t, err)
	g := Grant{Root: "/room/123/", Publish: []string{"/7"}, Subject: "42", Post: []string{"/comments"}}

	tok, err := Sign(key, g, time.Minute)
	require.NoError(t, err)
	parts := strings.Split(tok, ".")
	decode := func(seg string) map[string]any {
		raw, err := base64.RawURLEncoding.DecodeString(seg)
		require.NoError(t, err)
		var v map[string]any
		require.NoError(t, json.Unmarshal(raw, &v))
		return v
	}
	header, claims := decode(parts[0]), decode(parts[1])

	assert.Equal(t, map[string]any{"alg": "EdDSA", "kid": key.ID, "typ": "JWT"}, header)
	assert.Equal(t, "42", claims["sub"])
	assert.Equal(t, "/room/123/", claims["root"])
	assert.Equal(t, []any{"/7"}, claims["publish"])
	assert.Equal(t, []any{"/comments"}, claims["post"])
	for _, absent := range []string{"subscribe", "create", "redact", "path_auth", "scopes", "max_age", "MaxAge"} {
		assert.NotContains(t, claims, absent)
	}
}

func TestSign_Refusals(t *testing.T) {
	tests := map[string]struct {
		grant       Grant
		ttl         time.Duration
		wantErr     error
		wantErrText string
	}{
		"zero ttl":     {grant: Grant{Root: "/acme/app/"}, wantErrText: "ttl"},
		"ttl over 1 h": {grant: Grant{Root: "/acme/app/"}, ttl: 61 * time.Minute, wantErrText: "ttl"},
		// A verifier would refuse each, so it is never signed.
		"a max age": {
			grant: Grant{Grant: moqt.Grant{Root: "/acme/app/", Subscribe: []string{"/"}, MaxAge: time.Minute}},
			ttl:   time.Minute, wantErr: ErrInvalid, wantErrText: "max age",
		},
		"a root outside the key's prefix": {grant: Grant{Root: "/acme/apple/", Subscribe: []string{"/"}}, ttl: time.Minute, wantErr: ErrForbidden, wantErrText: "outside"},
		"a root above the key's prefix":   {grant: Grant{Root: "/acme/", Subscribe: []string{"/app/"}}, ttl: time.Minute, wantErr: ErrForbidden, wantErrText: "outside"},
		"patterns without a root":         {grant: Grant{Publish: []string{"/"}}, ttl: time.Minute, wantErr: ErrInvalid, wantErrText: "without a root"},
		"a bare root":                     {grant: Grant{Root: "acme/app"}, ttl: time.Minute, wantErr: ErrInvalid, wantErrText: "directory"},
		"a dot-dot root":                  {grant: Grant{Root: "/acme/app/../x/"}, ttl: time.Minute, wantErr: ErrInvalid, wantErrText: ".."},
		"a wildcard root":                 {grant: Grant{Root: "/acme/app/*/"}, ttl: time.Minute, wantErr: ErrInvalid, wantErrText: "*"},
		"a braced root":                   {grant: Grant{Root: "/acme/app/{x}/"}, ttl: time.Minute, wantErr: ErrInvalid, wantErrText: "{"},
		"a pattern with no slash":         {grant: Grant{Root: "/acme/app/", Subscribe: []string{"x"}}, ttl: time.Minute, wantErr: ErrInvalid, wantErrText: "subscribe"},
		"a malformed wildcard":            {grant: Grant{Root: "/acme/app/", Create: []string{"/{x"}}, ttl: time.Minute, wantErr: ErrInvalid, wantErrText: "create"},
		"a dot-dot pattern":               {grant: Grant{Root: "/acme/app/", Post: []string{"/x/../../y"}}, ttl: time.Minute, wantErr: ErrInvalid, wantErrText: ".."},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			key, err := GenerateKey("/acme/app/")
			require.NoError(t, err)

			_, err = Sign(key, tt.grant, tt.ttl)

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			}
			assert.ErrorContains(t, err, tt.wantErrText)
		})
	}
}

func TestGrant_Allows(t *testing.T) {
	room := Grant{
		Root: "/acme/room/42/", Publish: []string{"/7"}, Subscribe: []string{"/"},
		Create: []string{"/{user}/comments"},
		Post:   []string{"/comments", "/7/"},
		Redact: []string{"/comments", "/notes/{id...}"},
	}
	everything := Grant{Root: "/", Subscribe: []string{"/"}}
	pathAndBeneath := Grant{Root: "/acme/", Subscribe: []string{"/x", "/x/"}}
	tests := map[string]struct {
		grant  Grant
		action Action
		path   string
		want   bool
	}{
		"publish, the exact path":            {grant: room, action: ActionPublish, path: "/acme/room/42/7", want: true},
		"publish, beneath an exact path":     {grant: room, action: ActionPublish, path: "/acme/room/42/7/audio"},
		"publish, another path":              {grant: room, action: ActionPublish, path: "/acme/room/42/8"},
		"subscribe, beneath the root":        {grant: room, action: ActionSubscribe, path: "/acme/room/42/8", want: true},
		"subscribe, deep beneath the root":   {grant: room, action: ActionSubscribe, path: "/acme/room/42/8/audio", want: true},
		"subscribe, the root itself":         {grant: room, action: ActionSubscribe, path: "/acme/room/42"},
		"subscribe, a sibling of the root":   {grant: room, action: ActionSubscribe, path: "/acme/room/420/8"},
		"subscribe, above the root":          {grant: room, action: ActionSubscribe, path: "/acme/room"},
		"create, a wildcard segment":         {grant: room, action: ActionCreate, path: "/acme/room/42/alice/comments", want: true},
		"create, a wildcard of two segments": {grant: room, action: ActionCreate, path: "/acme/room/42/a/b/comments"},
		"post, the exact path":               {grant: room, action: ActionPost, path: "/acme/room/42/comments", want: true},
		"post, beneath a directory":          {grant: room, action: ActionPost, path: "/acme/room/42/7/audio", want: true},
		"post, the directory itself":         {grant: room, action: ActionPost, path: "/acme/room/42/7"},
		"redact, a second pattern":           {grant: room, action: ActionRedact, path: "/acme/room/42/notes/1/2", want: true},
		"an action with no patterns":         {grant: Grant{Root: "/acme/"}, action: ActionSubscribe, path: "/acme/x"},
		"an unknown action":                  {grant: room, action: "delete", path: "/acme/room/42/7"},
		"without the leading slash":          {grant: room, action: ActionPublish, path: "acme/room/42/7", want: true},
		"a doubled slash":                    {grant: room, action: ActionSubscribe, path: "/acme//room/42/8"},
		"a trailing slash":                   {grant: room, action: ActionPublish, path: "/acme/room/42/7/"},
		"a dot-dot path":                     {grant: room, action: ActionSubscribe, path: "/acme/room/42/../43/8"},
		"a braced path":                      {grant: room, action: ActionSubscribe, path: "/acme/room/42/{x}"},
		"the empty path":                     {grant: room, action: ActionSubscribe, path: ""},
		"the root of every path, beneath":    {grant: everything, action: ActionSubscribe, path: "/room/123", want: true},
		"the root of every path, itself":     {grant: everything, action: ActionSubscribe, path: "/", want: true},
		"the root of every path, unlisted":   {grant: everything, action: ActionPublish, path: "/room"},
		"the root of every path, respelled":  {grant: everything, action: ActionSubscribe, path: "/room//123"},
		"the zero grant":                     {grant: Grant{}, action: ActionSubscribe, path: "/room"},
		"an invalid pattern matches nothing": {grant: Grant{Root: "/acme/", Subscribe: []string{"x"}}, action: ActionSubscribe, path: "/acme/x"},
		"a path and beneath, the path":       {grant: pathAndBeneath, action: ActionSubscribe, path: "/acme/x", want: true},
		"a path and beneath, beneath it":     {grant: pathAndBeneath, action: ActionSubscribe, path: "/acme/x/y", want: true},
		"a path and beneath, a sibling":      {grant: pathAndBeneath, action: ActionSubscribe, path: "/acme/xy"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.grant.Allows(tt.action, tt.path))
		})
	}
}

func TestGrant_Patterns(t *testing.T) {
	g := Grant{
		Publish: []string{"/p"}, Subscribe: []string{"/s"},
		Create: []string{"/c"}, Post: []string{"/po"}, Redact: []string{"/r"},
	}
	tests := map[Action][]string{
		ActionPublish: {"/p"}, ActionSubscribe: {"/s"},
		ActionCreate: {"/c"}, ActionPost: {"/po"}, ActionRedact: {"/r"},
		"delete": nil,
	}
	for action, want := range tests {
		t.Run(string(action), func(t *testing.T) {
			assert.Equal(t, want, g.Patterns(action))
		})
	}
}
