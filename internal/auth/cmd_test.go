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
