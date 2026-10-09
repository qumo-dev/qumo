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
	key, err := token.GenerateKey("")
	require.NoError(tb, err)
	set, err := token.MarshalKeySet(key.Public())
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

// A path_auth credential is read as scopes: sub reads at its path and beneath
// it, and pub, being publish, never writes.
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

func TestAuthorizer_Scopes(t *testing.T) {
	v, key := newVerifier(t)
	chat := ingest.Track{BroadcastPath: "/room/123/comments", TrackName: "chat"}
	scope := func(broadcast string, prefix bool, track string, actions ...token.Action) token.Scope {
		return token.Scope{Actions: actions, Broadcast: broadcast, Prefix: prefix, Track: track}
	}
	bearerOf := func(subject string, scopes ...token.Scope) string {
		return "Bearer " + sign(t, key, token.Grant{Subject: subject, Scopes: scopes})
	}

	tests := map[string]struct {
		header     string
		access     ingest.Access
		wantSender string
		wantErr    bool
	}{
		"posting the exact track as a subject": {
			header:     bearerOf("42", scope("room/123/comments", false, "chat", token.ActionPost)),
			wantSender: "42",
		},
		"posting with no subject": {
			header: bearerOf("", scope("room/123/comments", false, "chat", token.ActionPost)),
		},
		"posting any track of the broadcast": {
			header:     bearerOf("42", scope("room/123/comments", false, "", token.ActionPost)),
			wantSender: "42",
		},
		"posting under a prefix": {
			header:     bearerOf("42", scope("room/123", true, "chat", token.ActionPost)),
			wantSender: "42",
		},
		"posting another track": {
			header:  bearerOf("42", scope("room/123/comments", false, "other", token.ActionPost)),
			wantErr: true,
		},
		"posting beneath the broadcast": {
			header:  bearerOf("42", scope("room/123/comments/user-42", false, "chat", token.ActionPost)),
			wantErr: true,
		},
		"posting under a sibling prefix": {
			header:  bearerOf("42", scope("room/12", true, "chat", token.ActionPost)),
			wantErr: true,
		},
		"posting with publish, which only sends through the relay": {
			header:  bearerOf("42", scope("room/123/comments", false, "chat", token.ActionPublish)),
			wantErr: true,
		},
		"posting with fetch only": {
			header:  bearerOf("42", scope("room/123/comments", false, "chat", token.ActionFetch)),
			wantErr: true,
		},
		"posting with subscribe only": {
			header:  bearerOf("42", scope("room/123/comments", false, "chat", token.ActionSubscribe)),
			wantErr: true,
		},
		"reading with fetch": {
			header: bearerOf("42", scope("room/123/comments", false, "chat", token.ActionFetch)),
			access: ingest.Read,
		},
		"reading under a fetch prefix": {
			header: bearerOf("", scope("room", true, "", token.ActionFetch)),
			access: ingest.Read,
		},
		"reading with subscribe only": {
			header:  bearerOf("42", scope("room/123/comments", false, "chat", token.ActionSubscribe)),
			access:  ingest.Read,
			wantErr: true,
		},
		"reading with post only": {
			header:  bearerOf("42", scope("room/123/comments", false, "chat", token.ActionPost)),
			access:  ingest.Read,
			wantErr: true,
		},
		"reading another track": {
			header:  bearerOf("42", scope("room/123/comments", false, "other", token.ActionFetch)),
			access:  ingest.Read,
			wantErr: true,
		},
		"the second of two scopes": {
			header: bearerOf("42",
				scope("room/9", true, "", token.ActionPost),
				scope("room/123/comments", false, "chat", token.ActionPost)),
			wantSender: "42",
		},
		"redacting the exact track": {
			header:     bearerOf("moderator", scope("room/123/comments", false, "chat", token.ActionRedact)),
			access:     ingest.Redact,
			wantSender: "moderator",
		},
		"redacting under a prefix, with no subject": {
			header: bearerOf("", scope("room", true, "", token.ActionRedact)),
			access: ingest.Redact,
		},
		"redacting another track": {
			header:  bearerOf("", scope("room/123/comments", false, "other", token.ActionRedact)),
			access:  ingest.Redact,
			wantErr: true,
		},
		"redacting with every other action": {
			header: bearerOf("", scope("room", true, "",
				token.ActionPublish, token.ActionSubscribe, token.ActionFetch, token.ActionPost)),
			access:  ingest.Redact,
			wantErr: true,
		},
		"posting with redact only": {
			header:  bearerOf("", scope("room", true, "", token.ActionRedact)),
			wantErr: true,
		},
		"reading with redact only": {
			header:  bearerOf("", scope("room", true, "", token.ActionRedact)),
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

func TestAuthorizer_NoVerifierChecksNothing(t *testing.T) {
	assert.Nil(t, authorizer(nil))
}
