package auth

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qumo-dev/qumo/token"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadServeConfig(t *testing.T) {
	tests := map[string]struct {
		addr        string
		keysFile    string
		want        serveConfig
		wantErrText string
	}{
		"default address": {keysFile: "keys.json", want: serveConfig{addr: defaultAddr, keysFile: "keys.json"}},
		"address set":     {addr: ":9000", keysFile: "keys.json", want: serveConfig{addr: ":9000", keysFile: "keys.json"}},
		"no key set":      {wantErrText: "QUMO_AUTH_KEYS_FILE"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("QUMO_AUTH_ADDR", tt.addr)
			t.Setenv("QUMO_AUTH_KEYS_FILE", tt.keysFile)

			got, err := loadServeConfig()

			if tt.wantErrText != "" {
				assert.ErrorContains(t, err, tt.wantErrText)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// The whole developer flow: keygen writes the two files, token signs with the
// private one, and an auth server loading the public one admits the token.
func TestRunKeygen_TokenAndServeAgree(t *testing.T) {
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
	assert.Contains(t, info.String(), "acme/app/alice/**", "the summary shows the grant as the server will")
	keys, err := token.LoadKeySet(pub)
	require.NoError(t, err)

	rec := post(t, &Handler{Keys: keys}, event(t, EventConnect, tok))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"publish":["acme/app/alice/**"]`)
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
