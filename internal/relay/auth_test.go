package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPattern_Covers(t *testing.T) {
	tests := map[string]struct {
		pattern string
		path    string
		want    bool
	}{
		"everything":                      {pattern: "**", path: "/any/where", want: true},
		"everything covers the root":      {pattern: "**", path: "/", want: true},
		"the base itself":                 {pattern: "acme/app/**", path: "/acme/app", want: true},
		"beneath the base":                {pattern: "acme/app/**", path: "/acme/app/room/1", want: true},
		"sibling sharing a string prefix": {pattern: "acme/app/**", path: "/acme/apple", want: false},
		"parent of the base":              {pattern: "acme/app/**", path: "/acme", want: false},
		"another tenant":                  {pattern: "acme/app/**", path: "/other/app", want: false},
		"case differs":                    {pattern: "acme/app/**", path: "/ACME/app", want: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p, err := parsePattern(tt.pattern)
			require.NoError(t, err)

			got := p.covers(moqt.BroadcastPath(tt.path))

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParsePattern_RefusesUnsupported(t *testing.T) {
	for _, raw := range []string{
		"",          // empty
		"acme",      // an exact path: not a subtree
		"acme/*",    // a segment wildcard
		"*/chat/**", // a wildcard inside the base
		"/acme/**",  // a leading slash makes an empty segment
		"acme//app/**",
		"acme/../other/**",
		"acme/./app/**",
	} {
		t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
			_, err := parsePattern(raw)

			assert.Error(t, err)
		})
	}
}

func TestParseGrant(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	tests := map[string]struct {
		body           string
		wantErr        error
		wantRefused    bool
		wantPublish    string // a path the grant must let publish, if set
		wantNotPublish string // a path it must not, if set
		wantExpires    time.Time
		wantRevalidate time.Duration
	}{
		"publish and subscribe with expiry": {
			body:           `{"publish":["acme/app/**"],"subscribe":["acme/**"],"expires":1000600,"revalidate":30}`,
			wantPublish:    "/acme/app/live",
			wantNotPublish: "/acme/other",
			wantExpires:    time.Unix(1_000_600, 0),
			wantRevalidate: 30 * time.Second,
		},
		"subscribe only": {
			body:           `{"subscribe":["**"]}`,
			wantNotPublish: "/acme/app",
		},
		"tier is ignored": {
			body:        `{"publish":["**"],"tier":"gold"}`,
			wantPublish: "/x",
		},
		"empty mounts are allowed":  {body: `{"publish":["**"],"mounts":{}}`, wantPublish: "/x"},
		"names nothing":             {body: `{"publish":[],"subscribe":[]}`, wantRefused: true},
		"no fields at all":          {body: `{}`, wantRefused: true},
		"expires already past":      {body: `{"publish":["**"],"expires":999999}`, wantErr: errInvalidGrant},
		"expires exactly now":       {body: `{"publish":["**"],"expires":1000000}`, wantErr: errInvalidGrant},
		"revalidate without expiry": {body: `{"publish":["**"],"revalidate":30}`, wantErr: errInvalidGrant},
		"zero revalidate":           {body: `{"publish":["**"],"expires":1000600,"revalidate":0}`, wantErr: errInvalidGrant},
		"root rewriting":            {body: `{"publish":["**"],"root":"acme"}`, wantErr: errInvalidGrant},
		"mounts":                    {body: `{"publish":["**"],"mounts":{".svc":".svc/p"}}`, wantErr: errInvalidGrant},
		"peer flag":                 {body: `{"publish":["**"],"peer":true}`, wantErr: errInvalidGrant},
		"non-subtree pattern":       {body: `{"publish":["acme/live"]}`, wantErr: errInvalidGrant},
		"not JSON":                  {body: `<html>`, wantErr: errInvalidGrant},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			g, err := parseGrant([]byte(tt.body), now)

			if tt.wantRefused {
				var refused refusedError
				require.ErrorAs(t, err, &refused)
				assert.Equal(t, http.StatusForbidden, refused.status)
				return
			}
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			if tt.wantPublish != "" {
				assert.True(t, g.mayPublish(moqt.BroadcastPath(tt.wantPublish)))
			}
			if tt.wantNotPublish != "" {
				assert.False(t, g.mayPublish(moqt.BroadcastPath(tt.wantNotPublish)))
			}
			assert.Equal(t, tt.wantExpires, g.expires)
			assert.Equal(t, tt.wantRevalidate, g.revalidate)
		})
	}
}

func TestSessionAuth_Connect(t *testing.T) {
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

			g, err := auth.connect(context.Background(), authRequest{ID: "00ff", Event: eventConnect, Path: "/acme"})

			if tt.wantStatus != 0 {
				require.Error(t, err)
				assert.Equal(t, tt.wantStatus, refusalStatus(err))
				return
			}
			require.NoError(t, err)
			assert.True(t, g.mayPublish(moqt.BroadcastPath(tt.wantPublish)))
		})
	}
}

func TestSessionAuth_Connect_SendsTheRequest(t *testing.T) {
	server := fakeAuthServer{body: `{"publish":["**"]}`}
	auth := server.start(t)
	req := authRequest{
		ID:         "00ff",
		Event:      eventConnect,
		Node:       "relay-1",
		Transport:  transportWebTransport,
		Remote:     "192.0.2.1:5000",
		Local:      "198.51.100.1:443",
		ServerName: "relay.example.com",
		Path:       "/acme/app",
		Query:      "jwt=a.b.c",
	}

	_, err := auth.connect(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, []authRequest{req}, server.received())
}

func TestSessionAuth_Connect_ServerDown(t *testing.T) {
	auth := (&fakeAuthServer{}).start(t)
	auth.endpoint = &url.URL{Scheme: "http", Host: "127.0.0.1:1"} // nothing listens on port 1

	_, err := auth.connect(context.Background(), authRequest{ID: "00ff", Event: eventConnect})

	require.Error(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, refusalStatus(err))
}

func TestSessionAuth_Connect_Public(t *testing.T) {
	patterns, err := parsePatterns([]string{"anon/**"})
	require.NoError(t, err)
	auth := &sessionAuth{public: &grant{publish: patterns, subscribe: patterns}}

	t.Run("admits without a server", func(t *testing.T) {
		g, err := auth.connect(context.Background(), authRequest{Path: "/"})

		require.NoError(t, err)
		assert.True(t, g.mayPublish("/anon/room"))
		assert.False(t, g.mayPublish("/other"))
	})
	t.Run("refuses a session that presents a credential", func(t *testing.T) {
		_, err := auth.connect(context.Background(), authRequest{Path: "/", Query: "jwt=a.b.c"})

		assert.ErrorIs(t, err, errCredentialOnPublic)
		assert.Equal(t, http.StatusUnauthorized, refusalStatus(err))
	})
}

func TestNewSessionAuth(t *testing.T) {
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
		"neither":                      {wantErr: errNoAuthSetting},
		"both":                         {url: "https://auth.example.com", public: "**", wantErr: errBothAuthSettings},
		"http off loopback":            {url: "http://auth.example.com/", wantErrText: "loopback"},
		"another scheme":               {url: "ftp://auth.example.com/", wantErrText: "https"},
		"bad public pattern":           {public: "anon", wantErrText: "QUMO_AUTH_PUBLIC"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("QUMO_AUTH_URL", tt.url)
			t.Setenv("QUMO_AUTH_PUBLIC", tt.public)

			auth, err := newSessionAuth()

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
	got := refusalStatus(errors.New("boom"))

	assert.Equal(t, http.StatusServiceUnavailable, got)
}
