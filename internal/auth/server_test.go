package auth

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/qumo-dev/qumo/token"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// post sends body to h as a relay would and returns the recorded response.
func post(tb testing.TB, h http.Handler, body string) *httptest.ResponseRecorder {
	tb.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	return rec
}

// event builds a relay session event carrying tok in its query.
func event(tb testing.TB, name, tok string) string {
	tb.Helper()
	q := ""
	if tok != "" {
		q = url.Values{"jwt": {tok}}.Encode()
	}
	b, err := json.Marshal(Request{
		ID: "00ff", Event: name, Node: "relay-1", Transport: TransportWebTransport,
		Remote: "192.0.2.1:5000", Path: "/acme/app", Query: q,
	})
	require.NoError(tb, err)
	return string(b)
}

// newKey returns a signing key confined to prefix and the key set an auth
// server trusts for it.
func newKey(tb testing.TB, prefix string) (token.SigningKey, map[string]token.Key) {
	tb.Helper()
	key, err := token.GenerateKey(prefix)
	require.NoError(tb, err)
	return key, map[string]token.Key{key.ID: key.Public()}
}

// sign mints a 10-minute token granting g.
func sign(tb testing.TB, key token.SigningKey, g token.Grant) string {
	tb.Helper()
	tok, err := token.Sign(key, g, 10*time.Minute)
	require.NoError(tb, err)
	return tok
}

func TestHandler_ServeHTTP_ConnectAndRevalidateGrant(t *testing.T) {
	key, keys := newKey(t, "acme/app")
	h := &Handler{Keys: keys, Revalidate: 30 * time.Second}
	tok := sign(t, key, token.Grant{Publish: "acme/app/alice", Subscribe: "acme/app"})

	for _, name := range []string{EventConnect, EventRevalidate} {
		t.Run(name, func(t *testing.T) {
			rec := post(t, h, event(t, name, tok))

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var g grantResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
			assert.Equal(t, []string{"acme/app/alice/**"}, g.Publish)
			assert.Equal(t, []string{"acme/app/**"}, g.Subscribe)
			assert.Equal(t, int64(30), g.Revalidate)
			assert.InDelta(t, time.Now().Add(10*time.Minute+token.Leeway).Unix(), g.Expires, 2, "exp plus the leeway")
		})
	}
}

// The relay parses the grant with its own decoder; what the auth server
// writes must be what the relay accepts.
func TestHandler_ServeHTTP_GrantParsesOnTheRelay(t *testing.T) {
	key, keys := newKey(t, "acme/app")
	h := &Handler{Keys: keys}

	rec := post(t, h, event(t, EventConnect, sign(t, key, token.Grant{Publish: "acme/app/alice", Subscribe: "acme/app"})))
	require.Equal(t, http.StatusOK, rec.Code)
	var g Grant
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))

	assert.True(t, g.Publish.Contains("/acme/app/alice/cam"))
	assert.False(t, g.Publish.Contains("/acme/app/bob"))
	assert.True(t, g.Subscribe.Contains("/acme/app/bob"))
	assert.Zero(t, g.Revalidate(), "static keys: no revalidate")
}

func TestHandler_ServeHTTP_Refusals(t *testing.T) {
	key, keys := newKey(t, "acme/app")
	stranger, _ := newKey(t, "")
	h := &Handler{Keys: keys}

	tests := map[string]struct {
		body       string
		wantStatus int
	}{
		"no credential":            {body: event(t, EventConnect, ""), wantStatus: http.StatusUnauthorized},
		"garbage credential":       {body: event(t, EventConnect, "not-a-token"), wantStatus: http.StatusUnauthorized},
		"an untrusted key":         {body: event(t, EventConnect, sign(t, stranger, token.Grant{Publish: "acme/app"})), wantStatus: http.StatusUnauthorized},
		"outside the key's prefix": {body: event(t, EventConnect, sign(t, key, token.Grant{Publish: "other/app"})), wantStatus: http.StatusForbidden},
		"unknown event":            {body: `{"id":"00ff","event":"announce"}`, wantStatus: http.StatusBadRequest},
		"not JSON":                 {body: `{`, wantStatus: http.StatusBadRequest},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			rec := post(t, h, tt.body)

			assert.Equal(t, tt.wantStatus, rec.Code)
		})
	}
}

func TestHandler_ServeHTTP_EndIsAcknowledged(t *testing.T) {
	h := &Handler{}

	rec := post(t, h, `{"id":"00ff","event":"end","reason":"closed","duration":12,"bytes":{"sent":10,"received":20}}`)

	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestHandler_ServeHTTP_OnlyPost(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, http.MethodPost, rec.Header().Get("Allow"))
}

func TestHandler_ServeHTTP_RefusalDoesNotEchoTheCredential(t *testing.T) {
	_, keys := newKey(t, "acme/app")
	stranger, _ := newKey(t, "")
	tok := sign(t, stranger, token.Grant{Publish: "acme/app"})
	h := &Handler{Keys: keys}

	rec := post(t, h, event(t, EventConnect, tok))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.NotContains(t, rec.Body.String(), tok)
}

func TestHandler_ServeHTTP_Anonymous(t *testing.T) {
	key, keys := newKey(t, "acme/app")
	stranger, _ := newKey(t, "")
	h := &Handler{Keys: keys, Anonymous: []string{"anon/**"}}
	tests := map[string]struct {
		body       string
		wantStatus int
		wantGrant  grantResponse
	}{
		"no credential gets the anonymous grant": {
			body:       event(t, EventConnect, ""),
			wantStatus: http.StatusOK,
			wantGrant:  grantResponse{Publish: []string{"anon/**"}, Subscribe: []string{"anon/**"}},
		},
		"a valid credential gets its own grant": {
			body:       event(t, EventConnect, sign(t, key, token.Grant{Publish: "acme/app"})),
			wantStatus: http.StatusOK,
		},
		"an invalid credential is refused, not granted anonymous": {
			body:       event(t, EventConnect, sign(t, stranger, token.Grant{Publish: "acme/app"})),
			wantStatus: http.StatusUnauthorized,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			rec := post(t, h, tt.body)

			require.Equal(t, tt.wantStatus, rec.Code, rec.Body.String())
			if tt.wantGrant.Publish != nil {
				var g grantResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
				assert.Equal(t, tt.wantGrant, g)
				assert.NotContains(t, rec.Body.String(), "expires", "an anonymous grant has no expiry")
			}
		})
	}
}

func TestHandler_ServeHTTP_NoAnonymousRefusesNoCredential(t *testing.T) {
	h := &Handler{}

	rec := post(t, h, event(t, EventConnect, ""))

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestParsePatternList(t *testing.T) {
	tests := map[string]struct {
		raw     string
		want    []string
		wantErr bool
	}{
		"one":               {raw: "anon/**", want: []string{"anon/**"}},
		"several, trimmed":  {raw: "anon/**, demo/**", want: []string{"anon/**", "demo/**"}},
		"everything":        {raw: "**", want: []string{"**"}},
		"exact path":        {raw: "anon", wantErr: true},
		"escaping segment":  {raw: "anon/../x/**", wantErr: true},
		"wildcard segment":  {raw: "*/live/**", wantErr: true},
		"leading slash":     {raw: "/anon/**", wantErr: true},
		"empty in the list": {raw: "anon/**,", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := parsePatternList(tt.raw)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
