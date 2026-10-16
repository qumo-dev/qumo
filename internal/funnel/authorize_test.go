package funnel

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/okdaichi/qumo-ledger/ingest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/token"
)

// newVerifier returns a Verifier trusting a fresh key, and the key.
func newVerifier(tb testing.TB) (*auth.Verifier, token.SigningKey) {
	tb.Helper()
	return newVerifierPausing(tb)
}

// newVerifierPausing returns a Verifier trusting a fresh key that pauses
// pause, and the key.
func newVerifierPausing(tb testing.TB, pause ...token.Action) (*auth.Verifier, token.SigningKey) {
	tb.Helper()
	key, err := token.GenerateKey("")
	require.NoError(tb, err)
	public := key.Public()
	public.Pause = pause
	set, err := token.MarshalKeySet(public)
	require.NoError(tb, err)
	keys := filepath.Join(tb.TempDir(), "keys.json")
	require.NoError(tb, os.WriteFile(keys, set, 0o600))
	v, err := auth.NewVerifier(auth.VerifierConfig{Keys: keys})
	require.NoError(tb, err)
	return v, key
}

func sign(tb testing.TB, key token.SigningKey, g token.Grant) string {
	tb.Helper()
	credential, err := token.Sign(key, g, time.Minute)
	require.NoError(tb, err)
	return credential
}

// signPathAuth mints a one-minute credential with a path_auth claim of pub
// and sub (each left out when empty), as other MoQ implementations sign one.
func signPathAuth(tb testing.TB, key token.SigningKey, pub, sub string) string {
	tb.Helper()
	pathAuth := map[string]string{"root": ""}
	if pub != "" {
		pathAuth["pub"] = pub
	}
	if sub != "" {
		pathAuth["sub"] = sub
	}
	now := time.Now().Unix()
	header, err := json.Marshal(map[string]string{"alg": "EdDSA", "kid": key.ID, "typ": "JWT"})
	require.NoError(tb, err)
	claims, err := json.Marshal(map[string]any{"path_auth": pathAuth, "iat": now, "nbf": now, "exp": now + 60})
	require.NoError(tb, err)
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key.Private, []byte(input)))
}

// A path_auth credential is read as the grant it amounts to: sub reads at its path and beneath
// it, and pub, being publish, which sends through a relay, permits nothing.
func TestAuthorizer_PathAuth(t *testing.T) {
	v, key := newVerifier(t)
	other, err := token.GenerateKey("")
	require.NoError(t, err)
	chat := ingest.Track{BroadcastPath: "/room/123/comments", TrackName: "comments"}

	pub := func(path string) string { return "Bearer " + signPathAuth(t, key, path, "") }
	sub := func(path string) string { return "Bearer " + signPathAuth(t, key, "", path) }

	tests := map[string]struct {
		header      string
		access      ingest.Access
		wantSender  string
		wantErr     bool
		wantUnauthn bool
	}{
		"writing with pub at the broadcast": {header: pub("room/123/comments"), wantErr: true},
		"writing with pub at every room":    {header: pub("room"), wantErr: true},
		"a subscribe-only credential":       {header: sub("room/123"), wantErr: true},
		"no credential":                     {wantErr: true, wantUnauthn: true},
		"not a bearer credential":           {header: "Basic YWxpY2U6c2VjcmV0", wantErr: true, wantUnauthn: true},
		"signed by an unknown key":          {header: "Bearer " + signPathAuth(t, other, "room/123", ""), wantErr: true, wantUnauthn: true},
		"not a credential at all":           {header: "Bearer hello", wantErr: true, wantUnauthn: true},
		"lower-case scheme":                 {header: "bearer " + signPathAuth(t, key, "", "room/123"), access: ingest.Read},
		"reading with a subscriber's":       {header: sub("room/123"), access: ingest.Read},
		"reading with a publisher's":        {header: pub("room/123"), access: ingest.Read, wantErr: true},
		"reading another room":              {header: sub("room/9"), access: ingest.Read, wantErr: true},
		"reading one segment beneath pub":   {header: pub("room/123/comments/user-42"), access: ingest.Read, wantErr: true},
		"creating with pub":                 {header: pub("room/123"), access: ingest.Create, wantErr: true},
		"creating with sub":                 {header: sub("room/123"), access: ingest.Create, wantErr: true},
	}
	authorize := authorizer(v)
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/tracks/room/123/comments/comments", nil)
			if tt.header != "" {
				r.Header.Set("Authorization", tt.header)
			}

			sender, err := authorize(r, chat, tt.access)

			if !tt.wantErr {
				require.NoError(t, err)
				assert.Equal(t, tt.wantSender, sender)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tt.wantUnauthn, errors.Is(err, ingest.ErrUnauthenticated))
		})
	}
}

func TestAuthorizer_Patterns(t *testing.T) {
	v, key := newVerifier(t)
	chat := ingest.Track{BroadcastPath: "/room/123/comments", TrackName: "chat"}
	// on grants actions on pattern beneath root.
	on := func(root, pattern string, actions ...token.Action) token.Grant {
		return grantOn(root, []string{pattern}, actions...)
	}
	bearerOf := func(subject string, g token.Grant) string {
		g.Subject = subject
		return "Bearer " + sign(t, key, g)
	}

	tests := map[string]struct {
		header     string
		access     ingest.Access
		wantSender string
		wantErr    bool
	}{
		"posting with no subject": {
			header: bearerOf("", on("room/123", "/comments", token.ActionPost)),
		},
		"posting as a subject": {
			header:     bearerOf("42", on("room/123", "/comments", token.ActionPost)),
			wantSender: "42",
		},
		"posting under a prefix": {
			header:     bearerOf("42", on("room/123", "/", token.ActionPost)),
			wantSender: "42",
		},
		"posting beneath the broadcast": {
			header:  bearerOf("42", on("room/123/comments", "/user-42", token.ActionPost)),
			wantErr: true,
		},
		"posting under a sibling prefix": {
			header:  bearerOf("42", on("room/12", "/", token.ActionPost)),
			wantErr: true,
		},
		"posting with publish, which sends through a relay": {
			header:  bearerOf("42", on("room/123", "/comments", token.ActionPublish)),
			wantErr: true,
		},
		"posting with subscribe only": {
			header:  bearerOf("42", on("room/123", "/comments", token.ActionSubscribe)),
			wantErr: true,
		},
		"reading with subscribe": {
			header: bearerOf("42", on("room/123", "/comments", token.ActionSubscribe)),
			access: ingest.Read,
		},
		"reading under a subscribe prefix": {
			header: bearerOf("", on("room", "/", token.ActionSubscribe)),
			access: ingest.Read,
		},
		"reading with publish, which sends through a relay": {
			header:  bearerOf("42", on("room/123", "/comments", token.ActionPublish)),
			access:  ingest.Read,
			wantErr: true,
		},
		"reading with post only": {
			header:  bearerOf("42", on("room/123", "/comments", token.ActionPost)),
			access:  ingest.Read,
			wantErr: true,
		},
		"creating with create": {
			header: bearerOf("app", on("room/123", "/comments", token.ActionCreate)),
			access: ingest.Create,
		},
		"creating under a create prefix": {
			header: bearerOf("", on("room", "/", token.ActionCreate)),
			access: ingest.Create,
		},
		"creating with publish, which sends through a relay": {
			header:  bearerOf("", on("room/123", "/comments", token.ActionPublish)),
			access:  ingest.Create,
			wantErr: true,
		},
		"posting with create only": {
			header:  bearerOf("42", on("room/123", "/comments", token.ActionCreate)),
			wantErr: true,
		},
		"creating with post only": {
			header:  bearerOf("42", on("room/123", "/comments", token.ActionPost)),
			access:  ingest.Create,
			wantErr: true,
		},
		"creating with every other action": {
			header: bearerOf("", on("room", "/",
				token.ActionPublish, token.ActionSubscribe, token.ActionPost, token.ActionRedact)),
			access:  ingest.Create,
			wantErr: true,
		},
		"the second of two patterns": {
			header:     bearerOf("42", grantOn("room", []string{"/9/", "/123/comments"}, token.ActionPost)),
			wantSender: "42",
		},
		"posting with a wildcard": {
			header:     bearerOf("42", on("room", "/{room}/comments", token.ActionPost)),
			wantSender: "42",
		},
		"posting at the root itself": {
			header:  bearerOf("42", on("room/123/comments", "/", token.ActionPost)),
			wantErr: true,
		},
		"redacting at the broadcast": {
			header:     bearerOf("moderator", on("room/123", "/comments", token.ActionRedact)),
			access:     ingest.Redact,
			wantSender: "moderator",
		},
		"redacting under a prefix, with no subject": {
			header: bearerOf("", on("room", "/", token.ActionRedact)),
			access: ingest.Redact,
		},
		"redacting with every other action": {
			header: bearerOf("", on("room", "/",
				token.ActionPublish, token.ActionSubscribe, token.ActionPost, token.ActionCreate)),
			access:  ingest.Redact,
			wantErr: true,
		},
		"posting with redact only": {
			header:  bearerOf("", on("room", "/", token.ActionRedact)),
			wantErr: true,
		},
		"reading with redact only": {
			header:  bearerOf("", on("room", "/", token.ActionRedact)),
			access:  ingest.Read,
			wantErr: true,
		},
	}
	authorize := authorizer(v)
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/tracks/room/123/comments/chat", nil)
			r.Header.Set("Authorization", tt.header)

			sender, err := authorize(r, chat, tt.access)

			if tt.wantErr {
				require.Error(t, err)
				assert.False(t, errors.Is(err, ingest.ErrUnauthenticated), "a valid credential that doesn't reach the track is forbidden")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantSender, sender)
		})
	}
}

// A key paused for publishing refuses creating tracks, and only that: the
// same credential still reads, posts and redacts in the tracks there are.
func TestAuthorizer_PausedKeyCreatesNothing(t *testing.T) {
	v, key := newVerifierPausing(t, token.ActionPublish)
	chat := ingest.Track{BroadcastPath: "/room/123/comments", TrackName: "chat"}
	app := "Bearer " + sign(t, key, grantOn("room/123", []string{"/comments"},
		token.ActionCreate, token.ActionPost, token.ActionSubscribe, token.ActionRedact))
	authorize := authorizer(v)

	for access, wantErr := range map[ingest.Access]bool{
		ingest.Create: true, ingest.Write: false, ingest.Read: false, ingest.Redact: false,
	} {
		r := httptest.NewRequest(http.MethodPost, "/tracks/room/123/comments/chat", nil)
		r.Header.Set("Authorization", app)

		_, err := authorize(r, chat, access)

		assert.Equal(t, wantErr, err != nil, "access %d: %v", access, err)
	}

	r := httptest.NewRequest(http.MethodPut, "/tracks/room/123/comments/chat", nil)
	r.Header.Set("Authorization", "Bearer "+sign(t, key, grantOn("room/123", []string{"/comments"}, token.ActionPost)))
	_, err := authorize(r, chat, ingest.Create)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "may not create", "a credential that may not create is refused for that, paused or not")
}

// With RELAY_PUBLISH, a track is created only beneath it, where the relay
// takes the funnel's announcements; other requests are left to authorize.
func TestPublishedBeneath(t *testing.T) {
	check := publishedBeneath("room", nil)
	r := httptest.NewRequest(http.MethodPut, "/tracks/x", nil)
	tests := map[string]struct {
		broadcast string
		access    ingest.Access
		wantErr   bool
	}{
		"creating beneath":          {broadcast: "/room/123/comments", access: ingest.Create},
		"creating at the path":      {broadcast: "/room", access: ingest.Create, wantErr: true},
		"creating beside it":        {broadcast: "/rooms/1", access: ingest.Create, wantErr: true},
		"creating elsewhere":        {broadcast: "/other/1", access: ingest.Create, wantErr: true},
		"posting elsewhere is left": {broadcast: "/other/1", access: ingest.Write},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := check(r, ingest.Track{BroadcastPath: tt.broadcast, TrackName: "chat"}, tt.access)

			assert.Equal(t, tt.wantErr, err != nil, "%v", err)
		})
	}
	assert.Nil(t, publishedBeneath("", nil), "no RELAY_PUBLISH checks nothing")
}

func TestAuthorizer_NoVerifierChecksNothing(t *testing.T) {
	assert.Nil(t, authorizer(nil))
}
