package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_Connect(t *testing.T) {
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

			g, err := auth.Connect(context.Background(), Request{ID: "00ff", Event: EventConnect, Path: "/acme"})

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

func TestClient_Connect_SendsTheRequest(t *testing.T) {
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

	_, err := auth.Connect(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, []Request{req}, server.received())
}

func TestClient_Connect_DoesNotFollowRedirects(t *testing.T) {
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

			_, err = c.Connect(context.Background(), Request{ID: "00ff", Event: EventConnect, Query: "jwt=a.b.c"})

			require.Error(t, err)
			assert.Equal(t, http.StatusServiceUnavailable, RefusalStatus(err))
			assert.Empty(t, elsewhere.received(), "the credential must not follow the redirect")
		})
	}
}

func TestClient_Connect_ServerDown(t *testing.T) {
	auth := (&fakeAuthServer{}).start(t)
	auth.endpoint = &url.URL{Scheme: "http", Host: "127.0.0.1:1"} // nothing listens on port 1

	_, err := auth.Connect(context.Background(), Request{ID: "00ff", Event: EventConnect})

	require.Error(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, RefusalStatus(err))
}

func TestLoadConfig(t *testing.T) {
	tests := map[string]struct {
		url         string
		public      string
		want        Config
		wantErrText string
	}{
		"auth server":        {url: "https://auth.example.com/v1/sessions", want: Config{URL: "https://auth.example.com/v1/sessions"}},
		"public grant":       {public: "anon/**, demo/**", want: Config{Public: Patterns{{base: "anon"}, {base: "demo"}}}},
		"neither":            {wantErrText: "neither"},
		"both":               {url: "https://auth.example.com", public: "**", wantErrText: "both set"},
		"bad public pattern": {public: "anon", wantErrText: "QUMO_AUTH_PUBLIC"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("QUMO_AUTH_URL", tt.url)
			t.Setenv("QUMO_AUTH_PUBLIC", tt.public)

			got, err := LoadConfig()

			if tt.wantErrText != "" {
				assert.ErrorContains(t, err, tt.wantErrText)
				return
			}
			require.NoError(t, err)
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
