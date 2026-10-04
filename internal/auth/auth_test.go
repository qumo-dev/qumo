package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_Authorize(t *testing.T) {
	tests := map[string]struct {
		server      *fakeAuthServer
		wantStatus  int // the HTTP status a WebTransport client would get; 0 means admitted
		wantPublish string
	}{
		"admitted": {
			server:      &fakeAuthServer{body: `{"publish":["acme/**"]}`},
			wantPublish: "/acme/live",
		},
		"401":             {server: &fakeAuthServer{status: http.StatusUnauthorized}, wantStatus: http.StatusUnauthorized},
		"403":             {server: &fakeAuthServer{status: http.StatusForbidden}, wantStatus: http.StatusForbidden},
		"empty grant":     {server: &fakeAuthServer{body: `{}`}, wantStatus: http.StatusForbidden},
		"server error":    {server: &fakeAuthServer{status: http.StatusInternalServerError}, wantStatus: http.StatusServiceUnavailable},
		"rate limited":    {server: &fakeAuthServer{status: http.StatusTooManyRequests}, wantStatus: http.StatusServiceUnavailable},
		"garbage body":    {server: &fakeAuthServer{body: `not json`}, wantStatus: http.StatusServiceUnavailable},
		"invalid grant":   {server: &fakeAuthServer{body: `{"publish":["**"],"root":"x"}`}, wantStatus: http.StatusServiceUnavailable},
		"redirect is 3xx": {server: &fakeAuthServer{status: http.StatusNotModified}, wantStatus: http.StatusServiceUnavailable},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			auth := tt.server.start(t)

			g, err := auth.Authorize(context.Background(), Request{ID: "00ff", Event: EventConnect, Path: "/acme"})

			if tt.wantStatus != 0 {
				require.Error(t, err)
				assert.Equal(t, tt.wantStatus, RefusalStatus(err))
				return
			}
			require.NoError(t, err)
			assert.True(t, g.Publish.Contains(moqt.BroadcastPath(tt.wantPublish)))
		})
	}
}

func TestClient_Authorize_SendsTheRequest(t *testing.T) {
	server := fakeAuthServer{body: `{"publish":["**"]}`}
	auth := server.start(t)
	req := Request{
		ID:         "00ff",
		Event:      EventConnect,
		Node:       "relay-1",
		Transport:  TransportWebTransport,
		Remote:     "192.0.2.1:5000",
		Local:      "198.51.100.1:443",
		ServerName: "relay.example.com",
		Path:       "/acme/app",
		Query:      "jwt=a.b.c",
	}

	_, err := auth.Authorize(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, []Request{req}, server.received())
}

func TestClient_Authorize_DoesNotFollowRedirects(t *testing.T) {
	elsewhere := &fakeAuthServer{body: `{"publish":["**"]}`}
	elsewhereURL := elsewhere.start(t).endpoint.String()
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, elsewhereURL, status)
			}))
			t.Cleanup(redirect.Close)
			c, err := NewClient(redirect.URL)
			require.NoError(t, err)

			_, err = c.Authorize(context.Background(), Request{ID: "00ff", Event: EventConnect, Query: "jwt=a.b.c"})

			require.Error(t, err)
			assert.Equal(t, http.StatusServiceUnavailable, RefusalStatus(err))
			assert.Empty(t, elsewhere.received(), "the credential must not follow the redirect")
		})
	}
}

func TestClient_Authorize_ServerDown(t *testing.T) {
	auth := (&fakeAuthServer{}).start(t)
	auth.endpoint = &url.URL{Scheme: "http", Host: "127.0.0.1:1"} // nothing listens on port 1

	_, err := auth.Authorize(context.Background(), Request{ID: "00ff", Event: EventConnect})

	require.Error(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, RefusalStatus(err))
}

func TestLoadConfig(t *testing.T) {
	tests := map[string]struct {
		url  string
		want Config
	}{
		"set":             {url: "http://127.0.0.1:4440/", want: Config{URL: "http://127.0.0.1:4440/"}},
		"unset: auth off": {url: "", want: Config{}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("QUMO_AUTH_URL", tt.url)

			got := LoadConfig()

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNewClient(t *testing.T) {
	tests := map[string]struct {
		url         string
		wantErrText string
	}{
		"https":             {url: "https://auth.example.com/v1/sessions"},
		"loopback http":     {url: "http://127.0.0.1:4440/"},
		"localhost http":    {url: "http://localhost:4440/"},
		"http off loopback": {url: "http://auth.example.com/", wantErrText: "loopback"},
		"another scheme":    {url: "ftp://auth.example.com/", wantErrText: "https"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c, err := NewClient(tt.url)

			if tt.wantErrText != "" {
				assert.ErrorContains(t, err, tt.wantErrText)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.url, c.endpoint.String())
		})
	}
}

func TestRefusalStatus_UnknownError(t *testing.T) {
	got := RefusalStatus(errors.New("boom"))

	assert.Equal(t, http.StatusServiceUnavailable, got)
}

func TestClient_End(t *testing.T) {
	req := Request{
		ID:       "00ff",
		Event:    EventEnd,
		Path:     "/acme",
		Bytes:    Bytes{Sent: 1500, Received: 300},
		Reason:   "closed",
		Duration: 42,
	}
	tests := map[string]struct {
		server  *fakeAuthServer
		wantErr bool
	}{
		"2xx":          {server: &fakeAuthServer{status: http.StatusNoContent}},
		"server error": {server: &fakeAuthServer{status: http.StatusInternalServerError}, wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			auth := tt.server.start(t)

			err := auth.End(context.Background(), req)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, []Request{req}, tt.server.received())
		})
	}
}

func TestClient_End_ServerDown(t *testing.T) {
	auth := (&fakeAuthServer{}).start(t)
	auth.endpoint = &url.URL{Scheme: "http", Host: "127.0.0.1:1"} // nothing listens on port 1

	assert.Error(t, auth.End(context.Background(), Request{ID: "00ff", Event: EventEnd}))
}

// TestRequest_MarshalJSON verifies the end-only members are left out of a
// connect request, and bytes is left out while both totals are zero.
func TestRequest_MarshalJSON(t *testing.T) {
	tests := map[string]struct {
		req  Request
		want string
	}{
		"connect": {
			req:  Request{ID: "00ff", Event: EventConnect, Transport: TransportQUIC, Path: "/acme"},
			want: `{"id":"00ff","event":"connect","transport":"quic","path":"/acme"}`,
		},
		"revalidate with no bytes yet": {
			req:  Request{ID: "00ff", Event: EventRevalidate, Transport: TransportQUIC, Path: "/acme"},
			want: `{"id":"00ff","event":"revalidate","transport":"quic","path":"/acme"}`,
		},
		"bytes in one direction": {
			req:  Request{ID: "00ff", Event: EventRevalidate, Transport: TransportQUIC, Path: "/acme", Bytes: Bytes{Received: 7}},
			want: `{"id":"00ff","event":"revalidate","transport":"quic","path":"/acme","bytes":{"sent":0,"received":7}}`,
		},
		"end": {
			req:  Request{ID: "00ff", Event: EventEnd, Transport: TransportQUIC, Path: "/acme", Bytes: Bytes{Sent: 9, Received: 1}, Reason: "dropped", Duration: 3},
			want: `{"id":"00ff","event":"end","transport":"quic","path":"/acme","bytes":{"sent":9,"received":1},"reason":"dropped","duration":3}`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := json.Marshal(tt.req)

			require.NoError(t, err)
			assert.JSONEq(t, tt.want, string(got))
		})
	}
}
