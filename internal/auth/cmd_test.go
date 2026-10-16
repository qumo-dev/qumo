package auth

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/token"
)

// The whole developer flow: keygen writes the two files, token signs with the
// private one, and a relay loading the public one admits the token.
func TestRunKeygen_TokenAndVerifierAgree(t *testing.T) {
	dir := t.TempDir()
	priv, pub := filepath.Join(dir, "signing-key.jwk"), filepath.Join(dir, "keys.json")
	var out bytes.Buffer

	require.NoError(t, runKeygen([]string{"-prefix", "acme/app", "-out", priv, "-keys", pub}, &out))
	assert.Contains(t, out.String(), "Key ID:")
	out.Reset()
	var info bytes.Buffer
	require.NoError(t, runToken([]string{"-key", priv, "-root", "/acme/app/",
		"-publish", "/alice", "-publish", "/alice/", "-subscribe", "/", "-ttl", "5m"}, &out, &info))
	tok := strings.TrimSpace(out.String())
	assert.Equal(t, 2, strings.Count(tok, "."), "stdout carries the token alone, so it can be captured")
	assert.Contains(t, info.String(), "Root:       acme/app\n", "the summary shows the grant as the relay will: normalized")
	assert.Contains(t, info.String(), "Publish:    /alice /alice/\n")
	v, err := NewVerifier(VerifierConfig{Keys: pub})
	require.NoError(t, err)

	g, err := v.Authorize(t.Context(), Request{ID: "s1", Event: EventConnect, Query: "jwt=" + tok})

	require.NoError(t, err)
	assert.True(t, g.Allows(token.ActionPublish, moqt.BroadcastPath("/acme/app/alice/cam")))
	assert.True(t, g.Allows(token.ActionPublish, moqt.BroadcastPath("/acme/app/alice")))
	assert.False(t, g.Allows(token.ActionPublish, moqt.BroadcastPath("/acme/app/bob")))
	assert.True(t, g.Allows(token.ActionSubscribe, moqt.BroadcastPath("/acme/app/bob")))
	assert.Equal(t, moqt.Grant{Root: "/acme/app/", Publish: []string{"/alice", "/alice/"}, Subscribe: []string{"/"}}, g.MoQ())
}

func TestRun_NoCommand(t *testing.T) {
	err := Run(nil)

	assert.ErrorContains(t, err, "a command is needed")
}

func TestRunKeygen_RefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	args := []string{"-out", filepath.Join(dir, "signing-key.jwk"), "-keys", filepath.Join(dir, "keys.json")}
	require.NoError(t, runKeygen(args, &bytes.Buffer{}))

	err := runKeygen(args, &bytes.Buffer{})

	assert.Error(t, err, "a second keygen must not replace the key every token was signed with")
}

// A keygen that can't write the key set leaves no private key behind, so a
// retry isn't refused by a key whose public half was never saved.
func TestRunKeygen_KeySetFailureRemovesThePrivateKey(t *testing.T) {
	dir := t.TempDir()
	priv, pub := filepath.Join(dir, "signing-key.jwk"), filepath.Join(dir, "keys.json")
	require.NoError(t, os.WriteFile(pub, []byte("{}"), 0o600))

	err := runKeygen([]string{"-out", priv, "-keys", pub}, &bytes.Buffer{})

	require.Error(t, err)
	assert.NoFileExists(t, priv)
}

func TestRun_UnknownCommand(t *testing.T) {
	err := Run([]string{"nope"})

	assert.ErrorContains(t, err, "unknown command")
}

// Each action's flag adds a pattern relative to -root, and may be repeated.
func TestRunToken_Patterns(t *testing.T) {
	dir := t.TempDir()
	priv, pub := filepath.Join(dir, "signing-key.jwk"), filepath.Join(dir, "keys.json")
	require.NoError(t, runKeygen([]string{"-prefix", "acme/app", "-out", priv, "-keys", pub}, &bytes.Buffer{}))
	var out, info bytes.Buffer

	require.NoError(t, runToken([]string{"-key", priv, "-sub", "42", "-root", "acme/app/room/1",
		"-publish", "/7", "-subscribe", "/", "-create", "/comments",
		"-post", "/comments", "-post", "/7/", "-post", "/8", "-redact", "/{user}/comments"}, &out, &info))

	assert.Contains(t, info.String(), "Root:       acme/app/room/1\n")
	assert.Contains(t, info.String(), "Publish:    /7\n")
	assert.Contains(t, info.String(), "Subscribe:  /\n")
	assert.Contains(t, info.String(), "Create:     /comments\n")
	assert.Contains(t, info.String(), "Post:       /comments /7/ /8\n")
	assert.Contains(t, info.String(), "Redact:     /{user}/comments\n")
	assert.Contains(t, info.String(), "Subject:    42\n")
	v, err := NewVerifier(VerifierConfig{Keys: pub})
	require.NoError(t, err)
	g, err := v.Authorize(t.Context(), Request{ID: "s1", Event: EventConnect, Query: "jwt=" + strings.TrimSpace(out.String())})
	require.NoError(t, err)
	assert.Equal(t, "42", g.Subject())
	tests := map[string]struct {
		action token.Action
		path   moqt.BroadcastPath
		want   bool
	}{
		"publish the exact path":       {action: token.ActionPublish, path: "/acme/app/room/1/7", want: true},
		"publish beneath it":           {action: token.ActionPublish, path: "/acme/app/room/1/7/x"},
		"subscribe beneath the root":   {action: token.ActionSubscribe, path: "/acme/app/room/1/9", want: true},
		"create":                       {action: token.ActionCreate, path: "/acme/app/room/1/comments", want: true},
		"post":                         {action: token.ActionPost, path: "/acme/app/room/1/comments", want: true},
		"post beneath a directory":     {action: token.ActionPost, path: "/acme/app/room/1/7/x", want: true},
		"post, a third pattern":        {action: token.ActionPost, path: "/acme/app/room/1/8", want: true},
		"post the directory itself":    {action: token.ActionPost, path: "/acme/app/room/1/7"},
		"redact, a wildcard":           {action: token.ActionRedact, path: "/acme/app/room/1/bob/comments", want: true},
		"redact, the wildcard missing": {action: token.ActionRedact, path: "/acme/app/room/1/comments"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, g.Allows(tt.action, tt.path))
		})
	}
}

func TestRunToken_Refusals(t *testing.T) {
	tests := map[string]struct {
		args    []string
		wantErr string
	}{
		"a root outside the key's prefix": {args: []string{"-root", "other", "-publish", "/"}, wantErr: "outside"},
		"a pattern with no slash":         {args: []string{"-root", "acme/app", "-subscribe", "live"}, wantErr: "subscribe"},
		"a malformed wildcard":            {args: []string{"-root", "acme/app", "-post", "/{x"}, wantErr: "post"},
		"the scope flag is gone":          {args: []string{"-scope", "publish:acme/app"}, wantErr: "-scope"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			priv := filepath.Join(dir, "signing-key.jwk")
			require.NoError(t, runKeygen([]string{"-prefix", "acme/app", "-out", priv, "-keys", filepath.Join(dir, "keys.json")}, &bytes.Buffer{}))
			var out bytes.Buffer

			err := runToken(append([]string{"-key", priv}, tt.args...), &out, &bytes.Buffer{})

			assert.ErrorContains(t, err, tt.wantErr)
			assert.Empty(t, out.String(), "no token is printed")
		})
	}
}

// A token of no patterns says so in the summary.
func TestRunToken_NoPatterns(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "signing-key.jwk")
	require.NoError(t, runKeygen([]string{"-prefix", "acme", "-out", priv, "-keys", filepath.Join(dir, "keys.json")}, &bytes.Buffer{}))
	var out, info bytes.Buffer

	require.NoError(t, runToken([]string{"-key", priv}, &out, &info))

	assert.Equal(t, 2, strings.Count(strings.TrimSpace(out.String()), "."))
	assert.Contains(t, info.String(), "nothing: the token names its key's tenant alone")
}

func TestRunKeygen_SaysWhatTheKeyGrants(t *testing.T) {
	tests := map[string]struct {
		prefix string
		want   []string
	}{
		"a prefix": {prefix: "acme/app", want: []string{"a root at or beneath acme/app", "-root acme/app -publish /demo"}},
		"none":     {want: []string{"any root (no prefix)", "-publish /demo"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			var out bytes.Buffer

			require.NoError(t, runKeygen([]string{"-prefix", tt.prefix,
				"-out", filepath.Join(dir, "signing-key.jwk"), "-keys", filepath.Join(dir, "keys.json")}, &out))

			for _, w := range tt.want {
				assert.Contains(t, out.String(), w)
			}
		})
	}
}
