package auth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/token"
)

func genKey(t *testing.T, prefix string) token.SigningKey {
	t.Helper()
	k, err := token.GenerateKey(prefix)
	require.NoError(t, err)
	return k
}

// keySetJSON is a JWK Set of keys, with publishing paused on those in
// pausedPublish.
func keySetJSON(t *testing.T, keys []token.SigningKey, pausedPublish ...token.SigningKey) []byte {
	t.Helper()
	public := make([]token.Key, len(keys))
	for i, k := range keys {
		public[i] = k.Public()
		if slices.ContainsFunc(pausedPublish, func(p token.SigningKey) bool { return p.ID == k.ID }) {
			public[i].Pause = []token.Action{token.ActionPublish}
		}
	}
	raw, err := token.MarshalKeySet(public...)
	require.NoError(t, err)
	return raw
}

func TestParseKeySet(t *testing.T) {
	active, limited := genKey(t, "acme/app"), genKey(t, "acme/app")

	set, err := parseKeySet(keySetJSON(t, []token.SigningKey{active, limited}, limited))
	require.NoError(t, err)
	assert.Len(t, set.keys, 2)
	assert.Equal(t, "/acme/app/", set.keys[active.ID].Prefix)
	assert.False(t, set.keys[active.ID].Pauses(token.ActionPublish))
	assert.True(t, set.keys[limited.ID].Pauses(token.ActionPublish))

	empty, err := parseKeySet([]byte(`{"keys":[]}`))
	require.NoError(t, err, "an empty set is valid: it admits nothing")
	assert.Empty(t, empty.keys)

	for name, raw := range map[string]string{
		"not JSON": `{`,
		"listed twice": func() string {
			one := keySetJSON(t, []token.SigningKey{active})
			var set struct{ Keys []json.RawMessage }
			require.NoError(t, json.Unmarshal(one, &set))
			return `{"keys":[` + string(set.Keys[0]) + `,` + string(set.Keys[0]) + `]}`
		}(),
		"private key":      `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo","d":"nWGxne_9WmC6hEr0kuwsxERJxWl7MmkZcDusAxyuf2A"}]}`,
		"unknown pause":    `{"keys":[` + keyEntry(t, active, map[string]any{"pause": []string{"publsh"}}) + `]}`,
		"pause not a list": `{"keys":[` + keyEntry(t, active, map[string]any{"pause": "publish"}) + `]}`,
		"pause named twice": `{"keys":[{"pause":["publish"],"pause":[],` +
			strings.TrimPrefix(keyEntry(t, active, nil), "{") + `]}`,
		"keys named twice":           `{"keys":[],"keys":[` + keyEntry(t, active, nil) + `]}`,
		"no keys member":             `{}`,
		"keys null":                  `{"keys":null}`,
		"keys in another case":       `{"Keys":[` + keyEntry(t, active, nil) + `]}`,
		"empty keys in another case": `{"Keys":[]}`,
	} {
		_, err := parseKeySet([]byte(raw))
		assert.Error(t, err, name)
	}
}

func TestParseKeySet_Pause(t *testing.T) {
	key := genKey(t, "acme/app")
	tests := map[string]struct {
		members map[string]any
		paused  bool
	}{
		"nothing":                         {members: nil, paused: false},
		"pause publish":                   {members: map[string]any{"pause": []string{"publish"}}, paused: true},
		"pause nothing":                   {members: map[string]any{"pause": []string{}}, paused: false},
		"names are case-sensitive":        {members: map[string]any{"Pause": []string{"publish"}}, paused: false},
		"another case can't unpause":      {members: map[string]any{"pause": []string{"publish"}, "Pause": []string{}}, paused: true},
		"unknown member is ignored":       {members: map[string]any{"paused": []string{"publish"}}, paused: false},
		"publish false is not a pause":    {members: map[string]any{"publish": false}, paused: false},
		"pause listed twice is one pause": {members: map[string]any{"pause": []string{"publish", "publish"}}, paused: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			set, err := parseKeySet([]byte(`{"keys":[` + keyEntry(t, key, tt.members) + `]}`))

			require.NoError(t, err)
			assert.Equal(t, tt.paused, set.keys[key.ID].Pauses(token.ActionPublish))
		})
	}
}

// verifierWith returns a Verifier whose key set is loaded as of loadedAt.
func verifierWith(t *testing.T, raw []byte, loadedAt time.Time) *Verifier {
	t.Helper()
	set, err := parseKeySet(raw)
	require.NoError(t, err)
	v := &Verifier{now: time.Now}
	v.store.replace(set, loadedAt)
	return v
}

func sessionReq(event, jwt string) Request {
	q := ""
	if jwt != "" {
		q = url.Values{"jwt": {jwt}}.Encode()
	}
	return Request{ID: "s1", Event: event, Transport: TransportWebTransport, Path: "/", Query: q}
}

func statusOf(err error) int {
	if err == nil {
		return http.StatusOK
	}
	if r, ok := errors.AsType[RefusedError](err); ok {
		return r.Status
	}
	return http.StatusServiceUnavailable
}

func TestVerifier_Grant(t *testing.T) {
	k := genKey(t, "acme/app")
	v := verifierWith(t, keySetJSON(t, []token.SigningKey{k}), time.Now())

	g, err := v.Authorize(context.Background(), sessionReq(EventConnect, sign(t, k, pathGrant("acme/app/live", "acme/app"))))
	require.NoError(t, err)
	assert.True(t, g.Allows(token.ActionPublish, moqt.BroadcastPath("/acme/app/live/room1")))
	assert.False(t, g.Allows(token.ActionPublish, moqt.BroadcastPath("/acme/app/other")))
	assert.True(t, g.Allows(token.ActionSubscribe, moqt.BroadcastPath("/acme/app/other")))
	assert.False(t, g.Allows(token.ActionSubscribe, moqt.BroadcastPath("/globex/app")))
	assert.Equal(t, revalidateEvery, g.Revalidate(), "every session is re-checked against the key set")
}

// TestVerifier_Expiry checks a credential after its expiry: refused at
// connect, while the live session it admitted continues.
func TestVerifier_Expiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k := genKey(t, "acme/app")
		v := verifierWith(t, keySetJSON(t, []token.SigningKey{k}), time.Now())
		jwt := sign(t, k, pathGrant("", "acme/app"))
		_, err := v.Authorize(context.Background(), sessionReq(EventConnect, jwt))
		require.NoError(t, err)

		time.Sleep(10*time.Minute + token.Leeway)

		_, err = v.Authorize(context.Background(), sessionReq(EventConnect, jwt))
		assert.Equal(t, http.StatusUnauthorized, statusOf(err), "connect: %v", err)
		_, err = v.Authorize(context.Background(), sessionReq(EventRevalidate, jwt))
		assert.NoError(t, err, "revalidate")
	})
}

func TestVerifier_Decisions(t *testing.T) {
	active, other := genKey(t, "acme/app"), genKey(t, "")
	v := verifierWith(t, keySetJSON(t, []token.SigningKey{active}), time.Now())
	unconfined := active
	unconfined.Prefix = "" // the app's copy of the key, unconfined: the relay's set confines it

	tests := map[string]struct {
		jwt            string
		wantConnect    int
		wantRevalidate int
	}{
		"active key":              {jwt: sign(t, active, pathGrant("", "acme/app")), wantConnect: 200, wantRevalidate: 200},
		"key not in the set":      {jwt: sign(t, other, pathGrant("", "acme/app")), wantConnect: 401, wantRevalidate: 401},
		"root outside the prefix": {jwt: sign(t, unconfined, publishAll("globex/app")), wantConnect: 403, wantRevalidate: 403},
		"prefix's sibling":        {jwt: sign(t, unconfined, publishAll("acme/application")), wantConnect: 403, wantRevalidate: 403},
		"root above the prefix":   {jwt: sign(t, unconfined, token.Grant{Root: "/acme/", Publish: []string{"/app/"}}), wantConnect: 403, wantRevalidate: 403},
		"no credential":           {jwt: "", wantConnect: 200, wantRevalidate: 200},
		"not a credential":        {jwt: "not.a.jwt", wantConnect: 401, wantRevalidate: 401},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := v.Authorize(context.Background(), sessionReq(EventConnect, tt.jwt))
			assert.Equal(t, tt.wantConnect, statusOf(err), "connect: %v", err)
			_, err = v.Authorize(context.Background(), sessionReq(EventRevalidate, tt.jwt))
			assert.Equal(t, tt.wantRevalidate, statusOf(err), "revalidate: %v", err)
		})
	}
}

func TestVerifier_FailStatic(t *testing.T) {
	k := genKey(t, "acme/app")
	jwt := sign(t, k, pathGrant("", "acme/app"))

	empty := &Verifier{now: time.Now}
	_, err := empty.Authorize(context.Background(), sessionReq(EventConnect, jwt))
	assert.Equal(t, http.StatusServiceUnavailable, statusOf(err), "no key set yet: connect refused as unavailable")
	_, err = empty.Authorize(context.Background(), sessionReq(EventRevalidate, jwt))
	_, refused := errors.AsType[RefusedError](err)
	assert.False(t, refused, "no key set: a revalidate is retried, not refused")

	stale := verifierWith(t, keySetJSON(t, []token.SigningKey{k}), time.Now().Add(-7*time.Hour))
	_, err = stale.Authorize(context.Background(), sessionReq(EventConnect, jwt))
	assert.Equal(t, http.StatusServiceUnavailable, statusOf(err), "stale: new sessions refused")
	_, err = stale.Authorize(context.Background(), sessionReq(EventRevalidate, jwt))
	assert.NoError(t, err, "stale: live sessions continue")
}

// TestVerifier_WithdrawnKeyEndsSessions checks that a key leaving the set
// ends its live sessions, before or after their credentials' expiry.
func TestVerifier_WithdrawnKeyEndsSessions(t *testing.T) {
	tests := map[string]struct {
		elapsed time.Duration
	}{
		"before its exp": {},
		"after its exp":  {elapsed: 10*time.Minute + token.Leeway},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				k := genKey(t, "acme/app")
				jwt := sign(t, k, pathGrant("", "acme/app"))
				v := verifierWith(t, keySetJSON(t, []token.SigningKey{k}), time.Now())
				_, err := v.Authorize(context.Background(), sessionReq(EventConnect, jwt))
				require.NoError(t, err)

				time.Sleep(tt.elapsed)
				set, err := parseKeySet([]byte(`{"keys":[]}`))
				require.NoError(t, err)
				v.store.replace(set, time.Now())

				_, err = v.Authorize(context.Background(), sessionReq(EventRevalidate, jwt))
				assert.Equal(t, http.StatusUnauthorized, statusOf(err))
			})
		})
	}
}

func TestNewVerifier(t *testing.T) {
	keysFile := writeKeySet(t, genKey(t, "acme/app"))
	badFile := filepath.Join(t.TempDir(), "bad.json")
	require.NoError(t, os.WriteFile(badFile, []byte("{"), 0o600))
	tests := map[string]struct {
		cfg     VerifierConfig
		wantErr string
	}{
		"file":                      {cfg: VerifierConfig{Keys: keysFile}},
		"missing file":              {cfg: VerifierConfig{Keys: filepath.Join(t.TempDir(), "nope.json")}, wantErr: "QUMO_AUTH_KEYS: key set"},
		"file that isn't a key set": {cfg: VerifierConfig{Keys: badFile}, wantErr: "QUMO_AUTH_KEYS"},
		"cache with a file":         {cfg: VerifierConfig{Keys: keysFile, KeysCache: "c.json"}, wantErr: "QUMO_AUTH_KEYS_CACHE"},
		"url with a cache":          {cfg: VerifierConfig{Keys: "https://keys.example.com/set", KeysCache: filepath.Join(t.TempDir(), "c.json")}},
		"url":                       {cfg: VerifierConfig{Keys: "https://keys.example.com/set", Token: "t"}},
		"loopback http":             {cfg: VerifierConfig{Keys: "http://127.0.0.1:8080/keys"}},
		"unset":                     {cfg: VerifierConfig{}, wantErr: "QUMO_AUTH_KEYS: not set"},
		"remote http":               {cfg: VerifierConfig{Keys: "http://keys.example.com"}, wantErr: "loopback"},
		"other scheme":              {cfg: VerifierConfig{Keys: "ftp://keys.example.com/set"}, wantErr: "want an https URL or a file path"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NewVerifier(tt.cfg)
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestURLKeySource(t *testing.T) {
	k := genKey(t, "acme/app")
	ks := &fakeKeyServer{body: keySetJSON(t, []token.SigningKey{k}), etag: `"e1"`}
	srv := httptest.NewServer(ks)
	defer srv.Close()
	src, err := newURLKeySource(srv.URL, "relay-token", nil)
	require.NoError(t, err)
	var store keyStore
	now := time.Now()

	require.NoError(t, src.refresh(context.Background(), &store, now))
	set, fresh := store.current(now)
	require.NotNil(t, set)
	assert.True(t, fresh)
	assert.Contains(t, set.keys, k.ID)
	assert.Equal(t, "Bearer relay-token", ks.gotAuth)

	later := now.Add(5 * time.Hour)
	require.NoError(t, src.refresh(context.Background(), &store, later))
	assert.Equal(t, `"e1"`, ks.gotMatch[len(ks.gotMatch)-1], "the ETag is sent back")
	got, fresh := store.current(later.Add(5 * time.Hour))
	assert.Same(t, set, got, "a 304 keeps the set")
	assert.True(t, fresh, "and counts as a success")

	ks.set(http.StatusInternalServerError, nil, "")
	assert.Error(t, src.refresh(context.Background(), &store, later.Add(time.Hour)))
	got, _ = store.current(later)
	assert.Same(t, set, got, "a failure keeps the set (fail-static)")

	ks.set(0, []byte(`{"keys":`), "")
	assert.Error(t, src.refresh(context.Background(), &store, later))
	got, _ = store.current(later)
	assert.Same(t, set, got, "an undecodable set is a failure too")
}

func TestURLKeySource_NoRedirect(t *testing.T) {
	target := &fakeKeyServer{}
	dest := httptest.NewServer(target)
	defer dest.Close()
	redirect := httptest.NewServer(http.RedirectHandler(dest.URL, http.StatusTemporaryRedirect))
	defer redirect.Close()
	src, err := newURLKeySource(redirect.URL, "relay-token", nil)
	require.NoError(t, err)

	err = src.refresh(context.Background(), &keyStore{}, time.Now())
	assert.ErrorContains(t, err, "307")
	assert.Empty(t, target.gotAuth, "the token isn't sent where the redirect points")
}

func TestFileKeySource(t *testing.T) {
	a, b := genKey(t, "acme/app"), genKey(t, "acme/app")
	path := filepath.Join(t.TempDir(), "keys.json")
	require.NoError(t, os.WriteFile(path, keySetJSON(t, []token.SigningKey{a}), 0o600))
	src := &fileKeySource{path: path}
	var store keyStore

	require.NoError(t, src.refresh(context.Background(), &store, time.Now()))
	set, _ := store.current(time.Now())
	assert.Contains(t, set.keys, a.ID)

	require.NoError(t, os.WriteFile(path, keySetJSON(t, []token.SigningKey{b}), 0o600))
	future := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(path, future, future))
	require.NoError(t, src.refresh(context.Background(), &store, time.Now()))
	set, _ = store.current(time.Now())
	assert.Contains(t, set.keys, b.ID, "a changed file is re-read")
	assert.NotContains(t, set.keys, a.ID)

	require.NoError(t, os.Remove(path))
	assert.Error(t, src.refresh(context.Background(), &store, time.Now()))
	set, _ = store.current(time.Now())
	assert.Contains(t, set.keys, b.ID, "a missing file keeps the last set")
}

func TestNextRefresh(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 200 {
		d := nextRefresh()
		assert.GreaterOrEqual(t, d, 27*time.Second)
		assert.LessOrEqual(t, d, 33*time.Second)
		seen[d] = true
	}
	assert.Greater(t, len(seen), 1, "the wait varies, so relays drift apart")
}

func TestKeySourceFor(t *testing.T) {
	tests := map[string]struct {
		value    string
		wantFile string
		wantURL  string
		wantErr  string
	}{
		"relative path":      {value: "keys.json", wantFile: "keys.json"},
		"absolute path":      {value: "/etc/qumo/keys.json", wantFile: "/etc/qumo/keys.json"},
		"windows path":       {value: `C:\qumo\keys.json`, wantFile: `C:\qumo\keys.json`},
		"file URL":           {value: "file:///etc/qumo/keys.json", wantFile: "/etc/qumo/keys.json"},
		"windows file URL":   {value: "file:///C:/qumo/keys.json", wantFile: "C:/qumo/keys.json"},
		"localhost file URL": {value: "file://localhost/etc/keys.json", wantFile: "/etc/keys.json"},
		"https":              {value: "https://keys.example.com/set", wantURL: "https://keys.example.com/set"},
		"uppercase scheme":   {value: "HTTPS://keys.example.com/set", wantURL: "HTTPS://keys.example.com/set"},
		"loopback http":      {value: "http://localhost:8080/keys", wantURL: "http://localhost:8080/keys"},
		"remote http":        {value: "http://keys.example.com/set", wantErr: "loopback"},
		"remote file host":   {value: "file://server/keys.json", wantErr: "local file"},
		"other scheme":       {value: "s3://bucket/keys.json", wantErr: `scheme "s3"`},
		"empty":              {value: "", wantErr: "not set"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			src, err := keySourceFor(tt.value, "t", "", nil)
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			switch s := src.(type) {
			case *fileKeySource:
				assert.Equal(t, tt.wantFile, s.path)
				assert.Empty(t, tt.wantURL)
			case *urlKeySource:
				assert.Equal(t, tt.wantURL, s.url)
				assert.Equal(t, "t", s.token)
				assert.Empty(t, tt.wantFile)
			}
		})
	}
}

// writeKeySet writes a key set file holding keys and returns its path.
func writeKeySet(t *testing.T, keys ...token.SigningKey) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys.json")
	require.NoError(t, os.WriteFile(path, keySetJSON(t, keys), 0o600))
	return path
}

// testRoot is the path of the root of the grants pathGrant makes: the prefix
// of the keys the tests sign with.
const testRoot = "acme/app"

// pathGrant grants, under testRoot, what a path_auth credential of pub and
// sub would: each non-empty path at or beneath testRoot and every path
// beneath it, publish for pub, and subscribe for sub.
func pathGrant(pub, sub string) token.Grant {
	g := token.Grant{Root: "/" + testRoot + "/"}
	if pub != "" {
		g.Publish = relAndBeneath(pub)
	}
	if sub != "" {
		g.Subscribe = relAndBeneath(sub)
	}
	return g
}

// relAndBeneath is the patterns relative to testRoot of path, which lies at
// or beneath it, and of every path beneath it: everything beneath the root
// for testRoot itself.
func relAndBeneath(path string) []string {
	rel := strings.TrimPrefix(path, testRoot)
	if rel == "" {
		return []string{"/"}
	}
	return []string{rel, rel + "/"}
}

// signPathAuth mints a 10-minute token with a path_auth claim of pub and sub
// (each left out when empty), as other MoQ implementations sign one: its root
// is the key's prefix, and pub and sub, at or beneath it, are relative to it.
func signPathAuth(tb testing.TB, key token.SigningKey, pub, sub string) string {
	tb.Helper()
	root := strings.Trim(key.Prefix, "/")
	pathAuth := map[string]string{"root": root}
	if pub != "" {
		pathAuth["pub"] = strings.TrimPrefix(pub, root)
	}
	if sub != "" {
		pathAuth["sub"] = strings.TrimPrefix(sub, root)
	}
	now := time.Now().Unix()
	header, err := json.Marshal(map[string]string{"alg": "EdDSA", "kid": key.ID, "typ": "JWT"})
	require.NoError(tb, err)
	claims, err := json.Marshal(map[string]any{"path_auth": pathAuth, "iat": now, "nbf": now, "exp": now + 600})
	require.NoError(tb, err)
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key.Private, []byte(input)))
}

// sign mints a 10-minute token granting g.
func sign(tb testing.TB, key token.SigningKey, g token.Grant) string {
	tb.Helper()
	tok, err := token.Sign(key, g, 10*time.Minute)
	require.NoError(tb, err)
	return tok
}

// publishAll grants publishing everything beneath root, a path.
func publishAll(root string) token.Grant {
	dir, err := token.Directory(root)
	if err != nil {
		panic(err)
	}
	return token.Grant{Root: dir, Publish: []string{"/"}}
}

// A path_auth credential, as other MoQ implementations sign it, is admitted
// with the grant it amounts to: pub publishes, and sub subscribes, on every
// track at or beneath each path. It names no subject.
func TestVerifier_PathAuthGrant(t *testing.T) {
	k := genKey(t, "acme/app")
	v := verifierWith(t, keySetJSON(t, []token.SigningKey{k}), time.Now())

	g, err := v.Authorize(context.Background(), sessionReq(EventConnect, signPathAuth(t, k, "acme/app/live", "acme/app/room")))

	require.NoError(t, err)
	assert.Empty(t, g.Subject())
	assert.True(t, g.Allows(token.ActionPublish, "/acme/app/live/cam"))
	assert.False(t, g.Allows(token.ActionPublish, "/acme/app/room"))
	assert.True(t, g.Allows(token.ActionSubscribe, "/acme/app/room/1"))
	assert.False(t, g.Allows(token.ActionSubscribe, "/acme/app/live"))
}

func TestVerifier_PatternGrant(t *testing.T) {
	k := genKey(t, "acme/app")
	v := verifierWith(t, keySetJSON(t, []token.SigningKey{k}), time.Now())
	jwt := sign(t, k, token.Grant{
		Root: "/acme/app/room/123/", Subject: "42",
		Publish: []string{"/comments"}, Subscribe: []string{"/"},
	})

	g, err := v.Authorize(context.Background(), sessionReq(EventConnect, jwt))

	require.NoError(t, err)
	assert.Equal(t, "42", g.Subject())
	assert.True(t, g.Allows(token.ActionPublish, "/acme/app/room/123/comments"))
	assert.False(t, g.Allows(token.ActionPublish, "/acme/app/room/123/comments/x"))
	assert.True(t, g.Allows(token.ActionSubscribe, "/acme/app/room/123/comments"))
	assert.False(t, g.Allows(token.ActionSubscribe, "/acme/app/room/1234/comments"))
}

// TestVerifier_PublishesBroadcast pins which grants have a publish pattern:
// create, post, redact and subscribe alone do not.
func TestVerifier_PublishesBroadcast(t *testing.T) {
	k := genKey(t, "acme/app")
	v := verifierWith(t, keySetJSON(t, []token.SigningKey{k}), time.Now())
	tests := map[string]struct {
		jwt  string
		want bool
	}{
		"publish":                            {jwt: sign(t, k, beneathLive(token.ActionPublish)), want: true},
		"publish and subscribe":              {jwt: sign(t, k, beneathLive(token.ActionPublish, token.ActionSubscribe)), want: true},
		"a path_auth publisher":              {jwt: signPathAuth(t, k, "acme/app/live", ""), want: true},
		"a path_auth of both":                {jwt: signPathAuth(t, k, "acme/app/live", "acme/app"), want: true},
		"subscribe":                          {jwt: sign(t, k, beneathLive(token.ActionSubscribe)), want: false},
		"a path_auth viewer":                 {jwt: signPathAuth(t, k, "", "acme/app"), want: false},
		"redact only":                        {jwt: sign(t, k, beneathLive(token.ActionRedact)), want: false},
		"post only":                          {jwt: sign(t, k, beneathLive(token.ActionPost)), want: false},
		"a viewer who may post":              {jwt: sign(t, k, beneathLive(token.ActionPost, token.ActionSubscribe)), want: false},
		"post and publish":                   {jwt: sign(t, k, beneathLive(token.ActionPost, token.ActionPublish)), want: true},
		"create only, which a relay ignores": {jwt: sign(t, k, beneathLive(token.ActionCreate)), want: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			g, err := v.decide(tt.jwt, nil, true, func(*Grant) bool { return false })

			require.NoError(t, err)
			assert.Equal(t, tt.want, publishesBroadcast(g.grant))
		})
	}
}

// A ticket is verified as a connect is, signed by the key that signed the
// session's own credential and by no other.
func TestVerifier_Ticket(t *testing.T) {
	session, other, gone := genKey(t, "acme/app"), genKey(t, "acme/app"), genKey(t, "acme/app")
	paused := genKey(t, "acme/app")
	v := verifierWith(t, keySetJSON(t, []token.SigningKey{session, other, paused}, paused), time.Now())
	listen := token.Grant{Root: "/acme/app/room/42/", Subscribe: []string{"/"}}
	speak := token.Grant{Root: "/acme/app/room/42/", Publish: []string{"/7"}}
	tests := map[string]struct {
		key    token.SigningKey
		ticket token.Grant
		keyID  string
		// published: the session has been granted what may publish.
		published bool
		wantCode  int
	}{
		"the session's key":                 {key: session, ticket: listen, keyID: session.ID, wantCode: http.StatusOK},
		"the session's key, to publish":     {key: session, ticket: speak, keyID: session.ID, wantCode: http.StatusOK},
		"another trusted key":               {key: other, ticket: listen, keyID: session.ID, wantCode: http.StatusForbidden},
		"a key not in the set":              {key: gone, ticket: listen, keyID: gone.ID, wantCode: http.StatusUnauthorized},
		"a session admitted by no key":      {key: session, ticket: listen, keyID: "", wantCode: http.StatusForbidden},
		"a paused key, a ticket to publish": {key: paused, ticket: speak, keyID: paused.ID, wantCode: http.StatusForbidden},
		"a paused key, a ticket to listen":  {key: paused, ticket: listen, keyID: paused.ID, wantCode: http.StatusOK},
		// A pause stops what is new: a publisher on air renews its grant.
		"a paused key, a publisher renewing": {key: paused, ticket: speak, keyID: paused.ID, published: true, wantCode: http.StatusOK},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			g, err := v.Ticket(sign(t, tt.key, tt.ticket), tt.keyID, tt.published)

			assert.Equal(t, tt.wantCode, statusOf(err), "%v", err)
			if tt.wantCode != http.StatusOK {
				assert.Nil(t, g)
				return
			}
			require.NotNil(t, g)
			assert.Equal(t, tt.keyID, g.KeyID())
			assert.WithinDuration(t, time.Now().Add(10*time.Minute), g.Expires(), 2*time.Second)
			assert.Equal(t, tt.ticket, g.grant)
		})
	}
}

// An expired ticket is refused when presented, while one already in force is
// still trusted: its expiry is the relay's to enforce, as MaxAge.
func TestVerifier_Ticket_Expired(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k := genKey(t, "acme/app")
		v := verifierWith(t, keySetJSON(t, []token.SigningKey{k}), time.Now())
		ticket := sign(t, k, pathGrant("", "acme/app/room"))

		time.Sleep(10*time.Minute + token.Leeway)

		g, err := v.Ticket(ticket, k.ID, false)
		assert.Equal(t, http.StatusUnauthorized, statusOf(err), "presented: %v", err)
		assert.Nil(t, g)
	})
}

// A ticket in force is trusted while its key is in the set and its root
// within the key's prefix, read from the set alone.
func TestVerifier_Trusts(t *testing.T) {
	k := genKey(t, "acme/app")
	v := verifierWith(t, keySetJSON(t, []token.SigningKey{k}), time.Now())
	g, err := v.Ticket(sign(t, k, pathGrant("", "acme/app/room")), k.ID, false)
	require.NoError(t, err)

	trusted, err := v.Trusts(g)
	require.NoError(t, err)
	assert.True(t, trusted, "its key is in the set")

	moved := k.Public()
	moved.Prefix = "other"
	set, err := token.MarshalKeySet(moved)
	require.NoError(t, err)
	parsed, err := parseKeySet(set)
	require.NoError(t, err)
	v.store.replace(parsed, time.Now())
	trusted, err = v.Trusts(g)
	require.NoError(t, err)
	assert.False(t, trusted, "its root is no longer within the key's prefix")

	empty, err := parseKeySet([]byte(`{"keys":[]}`))
	require.NoError(t, err)
	v.store.replace(empty, time.Now())
	trusted, err = v.Trusts(g)
	require.NoError(t, err)
	assert.False(t, trusted, "its key left the set")

	unloaded := &Verifier{now: time.Now}
	_, err = unloaded.Trusts(g)
	assert.Error(t, err, "with no set loaded, it can't be judged")
}

// A verified grant's MoQ grant is its token's, unchanged.
func TestVerifier_GrantMoQ(t *testing.T) {
	k := genKey(t, "acme/app")
	v := verifierWith(t, keySetJSON(t, []token.SigningKey{k}), time.Now())

	g, err := v.Authorize(context.Background(), sessionReq(EventConnect, sign(t, k, pathGrant("", "acme/app"))))

	require.NoError(t, err)
	assert.Equal(t, moqt.Grant{Root: "/acme/app/", Subscribe: []string{"/"}}, g.Token().Grant)
}

// Any token admits a session and is a ticket on a live one alike: what it
// is for is where it is presented.
func TestVerifier_TokenAdmitsAndIsATicket(t *testing.T) {
	k := genKey(t, "acme/app")
	v := verifierWith(t, keySetJSON(t, []token.SigningKey{k}), time.Now())
	jwt := sign(t, k, token.Grant{Root: "/acme/app/room/42/", Subscribe: []string{"/"}})

	_, err := v.Authorize(context.Background(), sessionReq(EventConnect, jwt))
	require.NoError(t, err, "at connect")
	_, err = v.Authorize(context.Background(), sessionReq(EventRevalidate, jwt))
	require.NoError(t, err, "at revalidate")
	g, err := v.Ticket(jwt, k.ID, false)
	require.NoError(t, err, "on a live session")
	assert.Equal(t, "/acme/app/room/42/", g.grant.Root)
}

// A session that connects with no credential is admitted anonymous and
// granted nothing, at connect and at each revalidate; one that connects with
// an empty credential is refused, as any credential that does not verify.
func TestVerifier_Anonymous(t *testing.T) {
	k := genKey(t, "acme/app")
	v := verifierWith(t, keySetJSON(t, []token.SigningKey{k}), time.Now())

	for _, event := range []string{EventConnect, EventRevalidate} {
		g, err := v.Authorize(context.Background(), sessionReq(event, ""))

		require.NoError(t, err, event)
		assert.True(t, g.Anonymous(), event)
		assert.Empty(t, g.KeyID(), event)
		assert.Zero(t, g.Expires(), event)
		assert.Equal(t, token.Grant{}, g.Token(), event)
		assert.Equal(t, revalidateEvery, g.Revalidate(), event)
	}

	req := sessionReq(EventConnect, "")
	req.Query = "jwt="
	_, err := v.Authorize(context.Background(), req)
	assert.Equal(t, http.StatusUnauthorized, statusOf(err))
}

// The first ticket of an anonymous session may be signed by any key in the
// set, verified as a connect is: a key paused for publishing refuses one
// that may publish, since the session has published nothing.
func TestVerifier_FirstTicket(t *testing.T) {
	session, other, gone := genKey(t, "acme/app"), genKey(t, "acme/app"), genKey(t, "acme/app")
	paused := genKey(t, "acme/app")
	v := verifierWith(t, keySetJSON(t, []token.SigningKey{session, other, paused}, paused), time.Now())
	listen := token.Grant{Root: "/acme/app/room/42/", Subscribe: []string{"/"}}
	speak := token.Grant{Root: "/acme/app/room/42/", Publish: []string{"/7"}}
	tests := map[string]struct {
		key      token.SigningKey
		ticket   token.Grant
		wantCode int
	}{
		"a trusted key":                     {key: session, ticket: listen, wantCode: http.StatusOK},
		"another trusted key":               {key: other, ticket: speak, wantCode: http.StatusOK},
		"a key not in the set":              {key: gone, ticket: listen, wantCode: http.StatusUnauthorized},
		"a paused key, a ticket to publish": {key: paused, ticket: speak, wantCode: http.StatusForbidden},
		"a paused key, a ticket to listen":  {key: paused, ticket: listen, wantCode: http.StatusOK},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			g, err := v.FirstTicket(sign(t, tt.key, tt.ticket))

			assert.Equal(t, tt.wantCode, statusOf(err), "%v", err)
			if tt.wantCode != http.StatusOK {
				assert.Nil(t, g)
				return
			}
			require.NotNil(t, g)
			assert.Equal(t, tt.key.ID, g.KeyID())
			assert.Equal(t, tt.ticket, g.grant)
		})
	}
}
