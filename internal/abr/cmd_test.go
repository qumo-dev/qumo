package abr

import (
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/token"
)

func TestRelayGrant(t *testing.T) {
	g := RelayGrant()
	assert.Equal(t, "/", g.Root)
	for _, path := range []string{"/.abr/live/cam1", "/live/cam1"} {
		assert.True(t, g.Allows(token.ActionSubscribe, path), "subscribe %s", path)
	}
	assert.True(t, g.Allows(token.ActionPublish, "/live/cam1.abr"))
}

func TestCheckSigningKey(t *testing.T) {
	whole, err := token.GenerateKey("/")
	require.NoError(t, err)
	assert.NoError(t, checkSigningKey(whole))

	tenant, err := token.GenerateKey("/acme/")
	require.NoError(t, err)
	assert.ErrorContains(t, checkSigningKey(tenant), "whole namespace",
		"a tenant's key cannot reach the opt-ins beneath /.abr/")
}

func TestRelayDialerTarget(t *testing.T) {
	t.Run("without a key, the URL as given", func(t *testing.T) {
		d := relayDialer{url: "https://relay:4433/?jwt=fixed"}
		target, lapses, err := d.target()
		require.NoError(t, err)
		assert.Equal(t, "https://relay:4433/?jwt=fixed", target)
		assert.True(t, lapses.IsZero())
	})

	t.Run("with a key, a fresh credential in place of ?jwt=", func(t *testing.T) {
		key, err := token.GenerateKey("/")
		require.NoError(t, err)
		d := relayDialer{url: "https://relay:4433/?jwt=stale&x=1", key: &key}
		before := time.Now()
		target, lapses, err := d.target()
		require.NoError(t, err)

		u, err := url.Parse(target)
		require.NoError(t, err)
		assert.Equal(t, "1", u.Query().Get("x"), "the rest of the query is kept")
		credential := u.Query().Get("jwt")
		require.NotEqual(t, "stale", credential)

		c, err := token.Verify(credential, map[string]token.Key{key.ID: key.Public()}, time.Now())
		require.NoError(t, err)
		assert.Equal(t, RelayGrant(), c.Grant)
		assert.WithinDuration(t, before.Add(token.MaxLifetime), lapses, time.Minute)
	})

	t.Run("an invalid URL does not echo its credential", func(t *testing.T) {
		key, err := token.GenerateKey("/")
		require.NoError(t, err)
		d := relayDialer{url: "https://relay:4433/%zz?jwt=secret", key: &key}
		_, _, err = d.target()
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "secret")
	})
}
