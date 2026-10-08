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
	require.NoError(t, runToken([]string{"-key", priv, "-publish", "acme/app/alice", "-subscribe", "acme/app", "-ttl", "5m"}, &out, &info))
	tok := strings.TrimSpace(out.String())
	assert.Equal(t, 2, strings.Count(tok, "."), "stdout carries the token alone, so it can be captured")
	assert.Contains(t, info.String(), "acme/app/alice/**", "the summary shows the grant as the relay will")
	v, err := NewVerifier(VerifierConfig{Keys: pub})
	require.NoError(t, err)

	g, err := v.Authorize(t.Context(), Request{ID: "s1", Event: EventConnect, Query: "jwt=" + tok})

	require.NoError(t, err)
	assert.True(t, g.Publish.Contains(moqt.BroadcastPath("/acme/app/alice/cam")))
	assert.False(t, g.Publish.Contains(moqt.BroadcastPath("/acme/app/bob")))
	assert.True(t, g.Subscribe.Contains(moqt.BroadcastPath("/acme/app/bob")))
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

func TestRunToken_Scopes(t *testing.T) {
	dir := t.TempDir()
	priv, pub := filepath.Join(dir, "signing-key.jwk"), filepath.Join(dir, "keys.json")
	require.NoError(t, runKeygen([]string{"-prefix", "acme/app", "-out", priv, "-keys", pub}, &bytes.Buffer{}))
	var out, info bytes.Buffer

	require.NoError(t, runToken([]string{"-key", priv, "-sub", "42",
		"-scope", "publish:acme/app/room/1/comments:chat",
		"-scope", "subscribe,fetch:acme/app/room/1/**"}, &out, &info))

	assert.Contains(t, info.String(), "publish acme/app/room/1/comments track chat")
	assert.Contains(t, info.String(), "subscribe,fetch acme/app/room/1/** track *")
	assert.Contains(t, info.String(), "42")
	v, err := NewVerifier(VerifierConfig{Keys: pub})
	require.NoError(t, err)
	g, err := v.Authorize(t.Context(), Request{ID: "s1", Event: EventConnect, Query: "jwt=" + strings.TrimSpace(out.String())})
	require.NoError(t, err)
	assert.Equal(t, "42", g.Subject())
	assert.True(t, g.Allows("publish", "/acme/app/room/1/comments", "chat"))
	assert.False(t, g.Allows("publish", "/acme/app/room/1/comments", "other"))
	assert.True(t, g.Allows("fetch", "/acme/app/room/1/comments", "other"))
}

func TestParseScope(t *testing.T) {
	tests := map[string]struct {
		value   string
		want    token.Scope
		wantErr bool
	}{
		"exact broadcast and track": {
			value: "publish:room/1:chat",
			want:  token.Scope{Actions: []token.Action{"publish"}, Broadcast: "room/1", Track: "chat"},
		},
		"a prefix, every track": {
			value: "subscribe,fetch:room/**",
			want:  token.Scope{Actions: []token.Action{"subscribe", "fetch"}, Broadcast: "room", Prefix: true},
		},
		"a track holding a colon": {
			value: "fetch:room/1:a:b",
			want:  token.Scope{Actions: []token.Action{"fetch"}, Broadcast: "room/1", Track: "a:b"},
		},
		"no broadcast":   {value: "publish", wantErr: true},
		"no actions":     {value: ":room/1", wantErr: true},
		"nothing given":  {value: "", wantErr: true},
		"an empty track": {value: "publish:room/1:", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := parseScope(tt.value)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRunToken_RefusesScopesWithPaths(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "signing-key.jwk")
	require.NoError(t, runKeygen([]string{"-out", priv, "-keys", filepath.Join(dir, "keys.json")}, &bytes.Buffer{}))

	err := runToken([]string{"-key", priv, "-publish", "a", "-scope", "fetch:a"}, &bytes.Buffer{}, &bytes.Buffer{})

	assert.ErrorContains(t, err, "both")
}
