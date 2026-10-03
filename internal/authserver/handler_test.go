package authserver

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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

// event builds a relay session event carrying token in its query.
func event(tb testing.TB, name, token string) string {
	tb.Helper()
	q := ""
	if token != "" {
		q = url.Values{"jwt": {token}}.Encode()
	}
	b, err := json.Marshal(map[string]any{
		"id": "00ff", "event": name, "node": "relay-1", "transport": "webtransport",
		"remote": "192.0.2.1:5000", "path": "/acme/app", "query": q,
	})
	require.NoError(tb, err)
	return string(b)
}

func TestHandler_ServeHTTP_ConnectAndRevalidateGrant(t *testing.T) {
	now := time.Now()
	signer := &fakeSigner{prefix: "acme/app"}
	h := &Handler{Keys: signer.keys(t), Revalidate: 30 * time.Second}
	token := signer.sign(t, validClaims(now))

	for _, name := range []string{eventConnect, eventRevalidate} {
		t.Run(name, func(t *testing.T) {
			rec := post(t, h, event(t, name, token))

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var g grant
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
			assert.Equal(t, []string{"acme/app/alice/**"}, g.Publish)
			assert.Equal(t, []string{"acme/app/**"}, g.Subscribe)
			assert.Equal(t, int64(30), g.Revalidate)
			assert.Greater(t, g.Expires, now.Unix())
		})
	}
}

func TestHandler_ServeHTTP_StaticKeysOmitRevalidate(t *testing.T) {
	signer := &fakeSigner{prefix: ""}
	h := &Handler{Keys: signer.keys(t)}

	rec := post(t, h, event(t, eventConnect, signer.sign(t, validClaims(time.Now()))))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "revalidate")
}

func TestHandler_ServeHTTP_Refusals(t *testing.T) {
	now := time.Now()
	signer := &fakeSigner{prefix: "acme/app"}
	outside := validClaims(now)
	outside["path_auth"] = map[string]any{"root": "other/app", "pub": ""}
	h := &Handler{Keys: signer.keys(t)}

	tests := map[string]struct {
		body       string
		wantStatus int
	}{
		"no credential":            {body: event(t, eventConnect, ""), wantStatus: http.StatusUnauthorized},
		"garbage credential":       {body: event(t, eventConnect, "not-a-token"), wantStatus: http.StatusUnauthorized},
		"outside the key's prefix": {body: event(t, eventConnect, signer.sign(t, outside)), wantStatus: http.StatusForbidden},
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
	signer := &fakeSigner{prefix: "acme/app"}
	other := &fakeSigner{prefix: ""}
	token := other.sign(t, validClaims(time.Now()))
	h := &Handler{Keys: signer.keys(t)}

	rec := post(t, h, event(t, eventConnect, token))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.NotContains(t, rec.Body.String(), token)
}

func TestHandler_ServeHTTP_Anonymous(t *testing.T) {
	signer := &fakeSigner{prefix: "acme/app"}
	h := &Handler{Keys: signer.keys(t), Anonymous: []string{"anon/**"}}
	other := &fakeSigner{prefix: ""}
	tests := map[string]struct {
		body       string
		wantStatus int
		wantGrant  grant
	}{
		"no credential gets the anonymous grant": {
			body:       event(t, eventConnect, ""),
			wantStatus: http.StatusOK,
			wantGrant:  grant{Publish: []string{"anon/**"}, Subscribe: []string{"anon/**"}},
		},
		"a valid credential gets its own grant": {
			body:       event(t, eventConnect, signer.sign(t, validClaims(time.Now()))),
			wantStatus: http.StatusOK,
		},
		"an invalid credential is refused, not granted anonymous": {
			body:       event(t, eventConnect, other.sign(t, validClaims(time.Now()))),
			wantStatus: http.StatusUnauthorized,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			rec := post(t, h, tt.body)

			require.Equal(t, tt.wantStatus, rec.Code, rec.Body.String())
			if tt.wantGrant.Publish != nil {
				var g grant
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
				assert.Equal(t, tt.wantGrant, g)
				assert.NotContains(t, rec.Body.String(), "expires", "an anonymous grant has no expiry")
			}
		})
	}
}

func TestHandler_ServeHTTP_NoAnonymousRefusesNoCredential(t *testing.T) {
	h := &Handler{}

	rec := post(t, h, event(t, eventConnect, ""))

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
