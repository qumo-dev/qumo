package auth

import (
	"context"
	"errors"
	"net/http"
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
			assert.True(t, g.MayPublish(moqt.BroadcastPath(tt.wantPublish)))
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

func TestClient_Connect_ServerDown(t *testing.T) {
	auth := (&fakeAuthServer{}).start(t)
	auth.endpoint = &url.URL{Scheme: "http", Host: "127.0.0.1:1"} // nothing listens on port 1

	_, err := auth.Connect(context.Background(), Request{ID: "00ff", Event: EventConnect})

	require.Error(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, RefusalStatus(err))
}

func TestClient_Connect_Public(t *testing.T) {
	patterns, err := parsePatterns([]string{"anon/**"})
	require.NoError(t, err)
	auth := &Client{public: &Grant{publish: patterns, subscribe: patterns}}

	t.Run("admits without a server", func(t *testing.T) {
		g, err := auth.Connect(context.Background(), Request{Path: "/"})

		require.NoError(t, err)
		assert.True(t, g.MayPublish("/anon/room"))
		assert.False(t, g.MayPublish("/other"))
	})
	t.Run("refuses a session that presents a credential", func(t *testing.T) {
		_, err := auth.Connect(context.Background(), Request{Path: "/", Query: "jwt=a.b.c"})

		assert.ErrorIs(t, err, ErrCredentialOnPublic)
		assert.Equal(t, http.StatusUnauthorized, RefusalStatus(err))
	})
}

func TestFromEnv(t *testing.T) {
	tests := map[string]struct {
		url         string
		public      string
		wantErr     error
		wantErrText string
		wantPublic  bool
	}{
		"auth server over https":       {url: "https://auth.example.com/v1/sessions"},
		"auth server on loopback http": {url: "http://127.0.0.1:4440/"},
		"auth server on localhost":     {url: "http://localhost:4440/"},
		"public grant":                 {public: "anon/**, demo/**", wantPublic: true},
		"neither":                      {wantErr: errNoSetting},
		"both":                         {url: "https://auth.example.com", public: "**", wantErr: errBothSettings},
		"http off loopback":            {url: "http://auth.example.com/", wantErrText: "loopback"},
		"another scheme":               {url: "ftp://auth.example.com/", wantErrText: "https"},
		"bad public pattern":           {public: "anon", wantErrText: "QUMO_AUTH_PUBLIC"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("QUMO_AUTH_URL", tt.url)
			t.Setenv("QUMO_AUTH_PUBLIC", tt.public)

			auth, err := FromEnv()

			switch {
			case tt.wantErr != nil:
				assert.ErrorIs(t, err, tt.wantErr)
			case tt.wantErrText != "":
				assert.ErrorContains(t, err, tt.wantErrText)
			default:
				require.NoError(t, err)
				assert.Equal(t, tt.wantPublic, auth.public != nil)
				if tt.url != "" {
					assert.Equal(t, tt.url, auth.endpoint.String())
				}
			}
		})
	}
}

func TestRefusalStatus_UnknownError(t *testing.T) {
	got := RefusalStatus(errors.New("boom"))

	assert.Equal(t, http.StatusServiceUnavailable, got)
}
