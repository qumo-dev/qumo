package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/token"
)

// TestURLKeySource_Cache pins what the cache is for: a relay that restarts
// while the key-set URL can't be reached still has its keys, for as long as
// the fail-static limit allows.
func TestURLKeySource_Cache(t *testing.T) {
	k := genKey(t, "acme/app")
	ks := &fakeKeyServer{body: keySetJSON(t, []token.SigningKey{k}), etag: `"e1"`}
	srv := httptest.NewServer(ks)
	cache := filepath.Join(t.TempDir(), "keys.cache.json")
	now := time.Now()

	first, err := keySourceFor(srv.URL, "t", cache)
	require.NoError(t, err)
	var store keyStore
	require.NoError(t, first.start(&store, now), "no cache yet is not an error")
	set, _ := store.current(now)
	assert.Nil(t, set)
	require.NoError(t, first.refresh(context.Background(), &store, now))
	root, err := os.OpenRoot(filepath.Dir(cache))
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()
	raw, err := root.ReadFile(filepath.Base(cache))
	require.NoError(t, err)
	assert.JSONEq(t, string(ks.body), string(raw), "the downloaded set is kept")

	srv.Close() // the URL can no longer be reached

	t.Run("a restarted relay loads the cache", func(t *testing.T) {
		restarted, err := keySourceFor(srv.URL, "t", cache)
		require.NoError(t, err)
		var fresh keyStore
		require.NoError(t, restarted.start(&fresh, now.Add(time.Minute)))
		set, ok := fresh.current(now.Add(time.Minute))
		require.NotNil(t, set)
		assert.True(t, ok)
		assert.Contains(t, set.keys, k.ID)
		assert.Error(t, restarted.refresh(context.Background(), &fresh, now.Add(time.Minute)))
		set, _ = fresh.current(now.Add(time.Minute))
		assert.NotNil(t, set, "a failed refresh keeps the cached set")
	})

	t.Run("the cache's age counts toward the fail-static limit", func(t *testing.T) {
		old := now.Add(-7 * time.Hour)
		require.NoError(t, os.Chtimes(cache, old, old))
		restarted, err := keySourceFor(srv.URL, "t", cache)
		require.NoError(t, err)
		var stale keyStore
		require.NoError(t, restarted.start(&stale, now))
		set, fresh := stale.current(now)
		assert.NotNil(t, set)
		assert.False(t, fresh, "a seven-hour-old cache starts no new sessions")
	})

	t.Run("a cache that isn't a key set is ignored", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "bad.json")
		require.NoError(t, os.WriteFile(bad, []byte("{"), 0o600))
		src, err := keySourceFor(srv.URL, "t", bad)
		require.NoError(t, err)
		var empty keyStore
		require.NoError(t, src.start(&empty, now))
		set, _ := empty.current(now)
		assert.Nil(t, set)
	})
}

func TestURLKeySource_UnchangedSetKeepsTheCacheFresh(t *testing.T) {
	k := genKey(t, "acme/app")
	srv := httptest.NewServer(&fakeKeyServer{body: keySetJSON(t, []token.SigningKey{k}), etag: `"e1"`})
	defer srv.Close()
	cache := filepath.Join(t.TempDir(), "keys.cache.json")
	src, err := keySourceFor(srv.URL, "t", cache)
	require.NoError(t, err)
	var store keyStore
	now := time.Now()
	require.NoError(t, src.refresh(context.Background(), &store, now))
	old := now.Add(-5 * time.Hour)
	require.NoError(t, os.Chtimes(cache, old, old))

	later := now.Add(time.Minute)
	require.NoError(t, src.refresh(context.Background(), &store, later)) // a 304

	info, err := os.Stat(cache)
	require.NoError(t, err)
	assert.WithinDuration(t, later, info.ModTime(), 2*time.Second, "a 304 confirms the cached set too")
}

func TestVerifier_PublishLimit(t *testing.T) {
	limited, free := genKey(t, "acme/app"), genKey(t, "acme/app")
	raw := []byte(`{"keys":[` +
		keyEntry(t, limited, map[string]any{"publish": false}) + `,` + keyEntry(t, free, nil) + `]}`)
	set, err := parseKeySet(raw)
	require.NoError(t, err)
	assert.True(t, set.pausedPublish[limited.ID])
	assert.False(t, set.pausedPublish[free.ID])
	v := verifierWith(t, raw, time.Now())

	tests := map[string]struct {
		jwt            string
		wantConnect    int
		wantRevalidate int
	}{
		"limited key, path_auth publisher": {jwt: signPathAuth(t, limited, "acme/app/live", ""), wantConnect: 403, wantRevalidate: 200},
		"limited key, path_auth both":      {jwt: signPathAuth(t, limited, "acme/app/live", "acme/app"), wantConnect: 403, wantRevalidate: 200},
		"limited key, path_auth viewer":    {jwt: signPathAuth(t, limited, "", "acme/app"), wantConnect: 200, wantRevalidate: 200},
		"another key, path_auth publisher": {jwt: signPathAuth(t, free, "acme/app/live", ""), wantConnect: 200, wantRevalidate: 200},
		"limited key, publish scope":       {jwt: sign(t, limited, scoped(token.ActionPublish)), wantConnect: 403, wantRevalidate: 200},
		// Writing one track into a broadcast someone else publishes, as a
		// viewer who may chat does, publishes no broadcast.
		"limited key, publish one track and subscribe": {
			jwt: sign(t, limited, token.Grant{Scopes: []token.Scope{
				{Actions: []token.Action{token.ActionPublish}, Broadcast: "acme/app/live", Track: "chat"},
				{Actions: []token.Action{token.ActionSubscribe}, Broadcast: "acme/app/live"},
			}}),
			wantConnect: 200, wantRevalidate: 200,
		},
		"limited key, subscribe and fetch scopes": {
			jwt: sign(t, limited, scoped(token.ActionSubscribe, token.ActionFetch)), wantConnect: 200, wantRevalidate: 200,
		},
		"another key, publish scope": {jwt: sign(t, free, scoped(token.ActionPublish)), wantConnect: 200, wantRevalidate: 200},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			jwt := tt.jwt
			_, err := v.Authorize(context.Background(), sessionReq(EventConnect, jwt))
			assert.Equal(t, tt.wantConnect, statusOf(err), "connect: %v", err)
			_, err = v.Authorize(context.Background(), sessionReq(EventRevalidate, jwt))
			assert.Equal(t, tt.wantRevalidate, statusOf(err), "a live session continues: %v", err)
		})
	}
}

// scoped is a grant of actions on every track beneath acme/app/live.
func scoped(actions ...token.Action) token.Grant {
	return token.Grant{Scopes: []token.Scope{{Actions: actions, Broadcast: "acme/app/live", Prefix: true}}}
}

// TestKeyStore_DeprecatedPublishWarning pins that a set pausing keys with the
// deprecated "publish": false is warned about once, naming the keys, and not
// again on refreshes that load the same keys.
func TestKeyStore_DeprecatedPublishWarning(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	limited, free := genKey(t, "acme/app"), genKey(t, "acme/app")
	deprecated := []byte(`{"keys":[` +
		keyEntry(t, limited, map[string]any{"publish": false}) + `,` + keyEntry(t, free, nil) + `]}`)
	var store keyStore
	replace := func(raw []byte) int {
		t.Helper()
		set, err := parseKeySet(raw)
		require.NoError(t, err)
		store.replace(set, time.Now())
		return strings.Count(logs.String(), "deprecated")
	}

	assert.Equal(t, 1, replace(deprecated), "first load")
	assert.Contains(t, logs.String(), limited.ID)
	assert.NotContains(t, logs.String(), free.ID)
	assert.Equal(t, 1, replace(deprecated), "a refresh to the same set")
	assert.Equal(t, 1, replace(keySetJSON(t, []token.SigningKey{limited, free}, limited)), "the new spelling")
	assert.Equal(t, 2, replace(deprecated), "the old spelling again")
}

// keyEntry is k's public JWK with the given members added.
func keyEntry(t *testing.T, k token.SigningKey, members map[string]any) string {
	t.Helper()
	var set struct {
		Keys []map[string]any `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(keySetJSON(t, []token.SigningKey{k}), &set))
	require.Len(t, set.Keys, 1)
	for name, v := range members {
		set.Keys[0][name] = v
	}
	raw, err := json.Marshal(set.Keys[0])
	require.NoError(t, err)
	return string(raw)
}
