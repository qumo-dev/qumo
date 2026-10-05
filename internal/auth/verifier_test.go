package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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

// keySetJSON is a JWK Set of keys, with "admit": false on those in noAdmit.
func keySetJSON(t *testing.T, keys []token.SigningKey, noAdmit ...token.SigningKey) []byte {
	t.Helper()
	public := make([]token.Key, len(keys))
	for i, k := range keys {
		public[i] = k.Public()
	}
	raw, err := token.MarshalKeySet(public...)
	require.NoError(t, err)
	if len(noAdmit) == 0 {
		return raw
	}
	var set struct {
		Keys []map[string]any `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(raw, &set))
	for _, entry := range set.Keys {
		for _, k := range noAdmit {
			if entry["kid"] == k.ID {
				entry["admit"] = false
			}
		}
	}
	out, err := json.Marshal(set)
	require.NoError(t, err)
	return out
}

func TestParseKeySet(t *testing.T) {
	active, retired := genKey(t, "acme/app"), genKey(t, "acme/app")

	set, err := parseKeySet(keySetJSON(t, []token.SigningKey{active, retired}, retired))
	require.NoError(t, err)
	assert.Len(t, set.keys, 2)
	assert.Equal(t, "acme/app", set.keys[active.ID].Prefix)
	assert.False(t, set.noAdmit[active.ID])
	assert.True(t, set.noAdmit[retired.ID])

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
		"private key": `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo","d":"nWGxne_9WmC6hEr0kuwsxERJxWl7MmkZcDusAxyuf2A"}]}`,
	} {
		_, err := parseKeySet([]byte(raw))
		assert.Error(t, err, name)
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

	g, err := v.Authorize(context.Background(), sessionReq(EventConnect, sign(t, k, token.Grant{Publish: "acme/app/live", Subscribe: "acme/app"})))
	require.NoError(t, err)
	assert.True(t, g.Publish.Contains(moqt.BroadcastPath("/acme/app/live/room1")))
	assert.False(t, g.Publish.Contains(moqt.BroadcastPath("/acme/app/other")))
	assert.True(t, g.Subscribe.Contains(moqt.BroadcastPath("/acme/app/other")))
	assert.False(t, g.Subscribe.Contains(moqt.BroadcastPath("/globex/app")))
	assert.Equal(t, revalidateEvery, g.Revalidate())
	assert.WithinDuration(t, time.Now().Add(10*time.Minute+token.Leeway), g.Expires(), 5*time.Second,
		"the session ends at the credential's exp plus the leeway")
}

func TestVerifier_Decisions(t *testing.T) {
	active, retired, other := genKey(t, "acme/app"), genKey(t, "acme/app"), genKey(t, "")
	v := verifierWith(t, keySetJSON(t, []token.SigningKey{active, retired}, retired), time.Now())
	unconfined := active
	unconfined.Prefix = "" // the app's copy of the key, unconfined: the relay's set confines it

	tests := map[string]struct {
		jwt            string
		wantConnect    int
		wantRevalidate int
	}{
		"active key":              {jwt: sign(t, active, token.Grant{Subscribe: "acme/app"}), wantConnect: 200, wantRevalidate: 200},
		"key that admits no new":  {jwt: sign(t, retired, token.Grant{Subscribe: "acme/app"}), wantConnect: 403, wantRevalidate: 200},
		"key not in the set":      {jwt: sign(t, other, token.Grant{Subscribe: "acme/app"}), wantConnect: 401, wantRevalidate: 401},
		"path outside the prefix": {jwt: sign(t, unconfined, token.Grant{Publish: "globex/app"}), wantConnect: 403, wantRevalidate: 403},
		"prefix's sibling":        {jwt: sign(t, unconfined, token.Grant{Publish: "acme/application"}), wantConnect: 403, wantRevalidate: 403},
		"no credential":           {jwt: "", wantConnect: 401, wantRevalidate: 401},
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
	jwt := sign(t, k, token.Grant{Subscribe: "acme/app"})

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

func TestVerifier_WithdrawnKeyEndsSessions(t *testing.T) {
	k := genKey(t, "acme/app")
	jwt := sign(t, k, token.Grant{Subscribe: "acme/app"})
	v := verifierWith(t, keySetJSON(t, []token.SigningKey{k}), time.Now())
	_, err := v.Authorize(context.Background(), sessionReq(EventConnect, jwt))
	require.NoError(t, err)

	set, err := parseKeySet([]byte(`{"keys":[]}`))
	require.NoError(t, err)
	v.store.replace(set, time.Now())

	_, err = v.Authorize(context.Background(), sessionReq(EventRevalidate, jwt))
	assert.Equal(t, http.StatusUnauthorized, statusOf(err))
}

func TestNewVerifier(t *testing.T) {
	tests := map[string]struct {
		cfg     VerifierConfig
		wantErr string
	}{
		"file":            {cfg: VerifierConfig{KeysFile: "keys.json"}},
		"url":             {cfg: VerifierConfig{KeysURL: "https://keys.example.com/set", UsageURL: "https://usage.example.com", Token: "t"}},
		"loopback http":   {cfg: VerifierConfig{KeysURL: "http://127.0.0.1:8080/keys"}},
		"both":            {cfg: VerifierConfig{KeysFile: "k", KeysURL: "https://x"}, wantErr: "both set"},
		"neither":         {cfg: VerifierConfig{}, wantErr: "no key set"},
		"remote http":     {cfg: VerifierConfig{KeysURL: "http://keys.example.com"}, wantErr: "loopback"},
		"usage over http": {cfg: VerifierConfig{KeysFile: "k", UsageURL: "http://usage.example.com"}, wantErr: "QUMO_USAGE_URL"},
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

// fakeKeyServer serves a key set with an ETag and records what it was sent.
// A zero value answers 200 with an empty set.
type fakeKeyServer struct {
	mu       sync.Mutex
	body     []byte
	etag     string
	status   int
	gotAuth  string
	gotMatch []string
}

func (f *fakeKeyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotAuth = r.Header.Get("Authorization")
	f.gotMatch = append(f.gotMatch, r.Header.Get("If-None-Match"))
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	if f.etag != "" {
		w.Header().Set("ETag", f.etag)
		if r.Header.Get("If-None-Match") == f.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	body := f.body
	if body == nil {
		body = []byte(`{"keys":[]}`)
	}
	_, _ = w.Write(body)
}

func (f *fakeKeyServer) set(status int, body []byte, etag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body, f.etag = status, body, etag
}

func TestURLKeySource(t *testing.T) {
	k := genKey(t, "acme/app")
	ks := &fakeKeyServer{body: keySetJSON(t, []token.SigningKey{k}), etag: `"e1"`}
	srv := httptest.NewServer(ks)
	defer srv.Close()
	src, err := newURLKeySource(srv.URL, "relay-token")
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
	src, err := newURLKeySource(redirect.URL, "relay-token")
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

// TestVerifier_ReportsUsage drives the Verifier as the relay does and checks
// the records that reach the usage URL.
func TestVerifier_ReportsUsage(t *testing.T) {
	k := genKey(t, "acme/app")
	sink := &fakeUsageSink{}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	v, err := NewVerifier(VerifierConfig{KeysFile: "unused", UsageURL: srv.URL, Token: "t"})
	require.NoError(t, err)
	set, err := parseKeySet(keySetJSON(t, []token.SigningKey{k}))
	require.NoError(t, err)
	v.store.replace(set, time.Now())
	jwt := sign(t, k, token.Grant{Publish: "acme/app/live", Subscribe: "acme/app"})

	_, err = v.Authorize(context.Background(), sessionReq(EventConnect, jwt))
	require.NoError(t, err)
	rv := sessionReq(EventRevalidate, jwt)
	rv.Bytes = Bytes{Sent: 10, Received: 20}
	_, err = v.Authorize(context.Background(), rv)
	require.NoError(t, err)
	end := sessionReq(EventEnd, jwt)
	end.Bytes, end.Reason = Bytes{Sent: 30, Received: 40}, "closed"
	require.NoError(t, v.End(context.Background(), end))
	require.NoError(t, v.usage.flush(context.Background()))

	batches := sink.received()
	require.Len(t, batches, 1)
	var types []string
	for _, r := range batches[0] {
		types = append(types, r.Type)
		assert.Equal(t, k.ID, r.KID)
		assert.Equal(t, roleBoth, r.Role)
		assert.NotEmpty(t, r.JTI)
	}
	assert.Equal(t, []string{recordSessionOpen, recordSessionClose}, types, "the close supersedes the pending usage")
	assert.Equal(t, map[string]int64{"gateway.ingress_bytes": 40, "gateway.egress_bytes": 30}, batches[0][1].Metrics,
		"what the relay received is ingress, what it sent egress")
	assert.Equal(t, "Bearer t", sink.gotAuth)
}

// fakeUsageSink records the batches POSTed to it and answers from a status
// queue: in order, the last repeating; empty means 200.
type fakeUsageSink struct {
	mu       sync.Mutex
	statuses []int
	batches  [][]usageRecord
	gotAuth  string
}

func (f *fakeUsageSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotAuth = r.Header.Get("Authorization")
	var batch []usageRecord
	if err := json.NewDecoder(r.Body).Decode(&batch); err == nil {
		f.batches = append(f.batches, batch)
	}
	status := http.StatusOK
	if len(f.statuses) > 0 {
		status = f.statuses[0]
		if len(f.statuses) > 1 {
			f.statuses = f.statuses[1:]
		}
	}
	w.WriteHeader(status)
}

func (f *fakeUsageSink) received() [][]usageRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]usageRecord(nil), f.batches...)
}

func newTestUsage(t *testing.T, sink *fakeUsageSink) *usageReporter {
	t.Helper()
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	r, err := newUsageReporter(srv.URL, "t")
	require.NoError(t, err)
	return r
}

var testUsageSession = usageSession{kid: "k1", jti: "user-42", role: rolePublish, expires: time.Now().Add(time.Hour)}

func TestUsageReporter(t *testing.T) {
	t.Run("usage is coalesced to the latest report", func(t *testing.T) {
		sink := &fakeUsageSink{}
		r := newTestUsage(t, sink)
		r.open("s1", testUsageSession, true)
		r.open("s1", testUsageSession, false) // a revalidate re-learning it: no second open
		r.reportUsage("s1", Bytes{Sent: 1, Received: 10})
		r.reportUsage("s1", Bytes{Sent: 2, Received: 20})
		require.NoError(t, r.flush(context.Background()))
		batches := sink.received()
		require.Len(t, batches, 1)
		require.Len(t, batches[0], 2)
		assert.Equal(t, recordSessionOpen, batches[0][0].Type)
		assert.Equal(t, int64(20), batches[0][1].Metrics["gateway.ingress_bytes"])
	})

	t.Run("a failed send is retried with newer usage", func(t *testing.T) {
		sink := &fakeUsageSink{statuses: []int{http.StatusBadGateway, http.StatusOK}}
		r := newTestUsage(t, sink)
		r.open("s1", testUsageSession, true)
		r.reportUsage("s1", Bytes{Received: 10})
		assert.Error(t, r.flush(context.Background()))
		r.reportUsage("s1", Bytes{Received: 20})
		require.NoError(t, r.flush(context.Background()))
		retry := sink.received()[1]
		require.Len(t, retry, 2)
		assert.Equal(t, recordSessionOpen, retry[0].Type, "the open is resent")
		assert.Equal(t, int64(20), retry[1].Metrics["gateway.ingress_bytes"], "the newer report wins")
	})

	t.Run("a rejected batch is dropped", func(t *testing.T) {
		sink := &fakeUsageSink{statuses: []int{http.StatusBadRequest, http.StatusOK}}
		r := newTestUsage(t, sink)
		r.open("s1", testUsageSession, true)
		require.NoError(t, r.flush(context.Background()))
		require.NoError(t, r.flush(context.Background()))
		assert.Len(t, sink.received(), 1)
	})

	t.Run("429 is retried", func(t *testing.T) {
		sink := &fakeUsageSink{statuses: []int{http.StatusTooManyRequests, http.StatusOK}}
		r := newTestUsage(t, sink)
		r.open("s1", testUsageSession, true)
		assert.Error(t, r.flush(context.Background()))
		require.NoError(t, r.flush(context.Background()))
		assert.Len(t, sink.received(), 2)
	})

	t.Run("an unknown session's end and usage are ignored", func(t *testing.T) {
		sink := &fakeUsageSink{}
		r := newTestUsage(t, sink)
		assert.False(t, r.close("nope", Bytes{Sent: 1}, ""))
		r.reportUsage("nope", Bytes{Sent: 1})
		require.NoError(t, r.flush(context.Background()))
		assert.Empty(t, sink.received(), "nothing pending, nothing sent")
	})

	t.Run("sessions whose end never came are forgotten after their expiry", func(t *testing.T) {
		r := newTestUsage(t, &fakeUsageSink{})
		r.open("old", usageSession{kid: "k1", role: rolePublish, expires: time.Now().Add(-sessionGrace - time.Second)}, true)
		r.open("live", testUsageSession, true)
		r.forgetExpired()
		assert.False(t, r.close("old", Bytes{}, ""))
		assert.True(t, r.close("live", Bytes{}, ""))
	})
}

func TestUsageRecordWire(t *testing.T) {
	raw, err := json.Marshal(usageRecord{Type: recordUsage, SessionID: "s1", KID: "k1", Role: roleSubscribe,
		Metrics: map[string]int64{"gateway.ingress_bytes": 1}, TS: "2026-10-05T00:00:00Z"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"usage","session_id":"s1","kid":"k1","role":"subscribe",`+
		`"metrics":{"gateway.ingress_bytes":1},"ts":"2026-10-05T00:00:00Z"}`, string(raw), "no jti or reason when empty")
	assert.False(t, strings.Contains(string(raw), "jti"))
}
