package funnel

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/token"
)

// writeSigningKey writes a fresh signing key confined to prefix and returns its
// file and the key.
func writeSigningKey(tb testing.TB, prefix string) (string, token.SigningKey) {
	tb.Helper()
	key, err := token.GenerateKey(prefix)
	require.NoError(tb, err)
	raw, err := key.MarshalJWK()
	require.NoError(tb, err)
	file := filepath.Join(tb.TempDir(), "signing-key.jwk")
	require.NoError(tb, os.WriteFile(file, raw, 0o600))
	return file, key
}

func TestNewUpstream(t *testing.T) {
	keyFile, _ := writeSigningKey(t, "")
	roomKeyFile, _ := writeSigningKey(t, "room")
	tests := map[string]struct {
		cfg     RelayConfig
		wantErr string
	}{
		"a URL alone":                 {cfg: RelayConfig{URL: "https://relay:4433/?jwt=abc"}},
		"a signing key":               {cfg: RelayConfig{URL: "moqt://relay:4433", SigningKeyFile: keyFile, Publish: "room"}},
		"an unparsable URL":           {cfg: RelayConfig{URL: "https://relay:port"}, wantErr: "RELAY_URL"},
		"a key without a path":        {cfg: RelayConfig{URL: "https://relay", SigningKeyFile: keyFile}, wantErr: "set together"},
		"a path without a key":        {cfg: RelayConfig{URL: "https://relay", Publish: "room"}, wantErr: "set together"},
		"a missing key file":          {cfg: RelayConfig{URL: "https://relay", SigningKeyFile: "no-such.jwk", Publish: "room"}, wantErr: "RELAY_SIGNING_KEY"},
		"a path outside the key":      {cfg: RelayConfig{URL: "https://relay", SigningKeyFile: roomKeyFile, Publish: "live"}, wantErr: "RELAY_PUBLISH"},
		"a CA file that is not there": {cfg: RelayConfig{URL: "https://relay", CAFile: "no-such.pem"}, wantErr: "relay TLS"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			u, err := newUpstream(tt.cfg)

			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				assert.Nil(t, u)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, u)
		})
	}
}

func TestUpstream_Target(t *testing.T) {
	t.Run("the URL as given", func(t *testing.T) {
		u, err := newUpstream(RelayConfig{URL: "https://relay:4433/?jwt=given"})
		require.NoError(t, err)

		target, err := u.target()

		require.NoError(t, err)
		assert.Equal(t, "https://relay:4433/?jwt=given", target)
	})

	t.Run("a fresh credential on every dial", func(t *testing.T) {
		keyFile, key := writeSigningKey(t, "")
		u, err := newUpstream(RelayConfig{URL: "moqt://relay:4433/?jwt=stale", SigningKeyFile: keyFile, Publish: "/room"})
		require.NoError(t, err)

		first, err := u.target()
		require.NoError(t, err)
		second, err := u.target()
		require.NoError(t, err)

		assert.NotEqual(t, first, second, "each dial signs its own credential")
		parsed, err := url.Parse(first)
		require.NoError(t, err)
		assert.Equal(t, "relay:4433", parsed.Host)
		claims, err := token.Verify(parsed.Query().Get("jwt"), map[string]token.Key{key.ID: key.Public()}, time.Now())
		require.NoError(t, err, "the ?jwt= given is replaced by a signed one")
		assert.Equal(t, []token.Scope{
			{Actions: []token.Action{token.ActionPublish}, Broadcast: "room", Prefix: true},
		}, claims.Scopes, "publishing every track at or beneath RELAY_PUBLISH")
		assert.Empty(t, claims.Subject)
	})
}

func TestRelayName(t *testing.T) {
	tests := map[string]struct {
		url  string
		want string
	}{
		"none":               {url: "", want: "none"},
		"the credential hid": {url: "https://relay:4433/moq?jwt=secret", want: "https://relay:4433/moq"},
		"an unparsable URL":  {url: "https://relay:port", want: "invalid"},
		"native QUIC":        {url: "moqt://relay:4433", want: "moqt://relay:4433"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, relayName(tt.url))
		})
	}
}
