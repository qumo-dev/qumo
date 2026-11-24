package abr

import (
	"net/url"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/relaycred"
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

// An opt-in names its broadcast only when spelled exactly as that
// broadcast's opt-in: a relay grants and routes exact paths, so one that
// cleans to another broadcast (a ".." climbing out of the publisher's own
// directory) is refused rather than read with the transcoder's grant.
func TestOptedIn(t *testing.T) {
	tests := map[string]struct {
		optIn  moqt.BroadcastPath
		want   moqt.BroadcastPath
		wantOK bool
	}{
		"a broadcast":                   {optIn: "/.abr/live/cam1", want: "/live/cam1", wantOK: true},
		"a single segment":              {optIn: "/.abr/cam", want: "/cam", wantOK: true},
		"the bare directory":            {optIn: "/.abr", want: "/", wantOK: true},
		"the directory, trailing slash": {optIn: "/.abr/", want: "/", wantOK: true},
		"a trailing slash":              {optIn: "/.abr/live/cam1/", want: "/live/cam1", wantOK: false},
		"a doubled slash":               {optIn: "/.abr/live//cam1", want: "/live/cam1", wantOK: false},
		"a dot segment":                 {optIn: "/.abr/live/./cam1", want: "", wantOK: false},
		"climbing into another's":       {optIn: "/.abr/acme/../victim/cam", want: "", wantOK: false},
		"climbing out of the directory": {optIn: "/.abr/../victim/cam", want: "", wantOK: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := optedIn(tt.optIn)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantOK, ok)
		})
	}
}

func TestRelayDialerTarget(t *testing.T) {
	t.Run("with a key, a fresh credential in place of ?jwt=", func(t *testing.T) {
		key, err := token.GenerateKey("/")
		require.NoError(t, err)
		before := time.Now()
		target, lapses, err := relaycred.Target("https://relay:4433/?jwt=stale&x=1", key, RelayGrant())
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
}
