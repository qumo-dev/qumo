package auth

import (
	"bytes"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qumo-dev/qumo/token"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadServeConfig(t *testing.T) {
	tests := map[string]struct {
		keysFile    string
		anonymous   string
		want        serveConfig
		wantErrText string
	}{
		"keys only":      {keysFile: "keys.json", want: serveConfig{addr: defaultAddr, keysFile: "keys.json"}},
		"anonymous only": {anonymous: "anon/**", want: serveConfig{addr: defaultAddr, anonymous: []string{"anon/**"}}},
		"both":           {keysFile: "keys.json", anonymous: "**", want: serveConfig{addr: defaultAddr, keysFile: "keys.json", anonymous: []string{"**"}}},
		"neither":        {wantErrText: "neither"},
		"bad pattern":    {anonymous: "anon", wantErrText: "QUMO_AUTH_ANONYMOUS"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("QUMO_AUTH_ADDR", "")
			t.Setenv("QUMO_AUTH_KEYS_FILE", tt.keysFile)
			t.Setenv("QUMO_AUTH_ANONYMOUS", tt.anonymous)

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
func TestKeygenTokenServe_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	priv, pub := filepath.Join(dir, "signing-key.jwk"), filepath.Join(dir, "keys.json")
	var out bytes.Buffer

	require.NoError(t, runKeygen([]string{"-prefix", "acme/app", "-out", priv, "-keys", pub}, &out))
	assert.Contains(t, out.String(), "kid:")
	out.Reset()
	require.NoError(t, runToken([]string{"-key", priv, "-publish", "acme/app/alice", "-subscribe", "acme/app", "-ttl", "5m"}, &out))
	tok := strings.TrimSpace(out.String())
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

func TestRun_UnknownCommand(t *testing.T) {
	err := Run([]string{"nope"})

	assert.ErrorContains(t, err, "unknown command")
}
