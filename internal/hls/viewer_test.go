package hls

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/token"
)

// newViewerMux returns a mux checking viewers with check, carrying param, with
// /live/cam1 live and its video track served by a handler that echoes the
// path it got.
func newViewerMux(tb testing.TB, param string, check *fakeViewerCheck) *streamMux {
	tb.Helper()
	access := viewerAccess{param: param}
	if check != nil {
		access.check = check.check
	}
	mux := newStreamMux(10*time.Second, access)
	live := &liveness{}
	live.mark(time.Now())
	mux.RegisterBroadcast("/live/cam1").AddTrack(trackEntry{
		info: trackInfo{Name: "video", Role: "video", Width: 1280, Height: 720},
		handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Served-Path", r.URL.Path)
		}),
		live: live,
	})
	return mux
}

func serve(mux *streamMux, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestStreamMux_ViewerAccess(t *testing.T) {
	targets := []string{
		"/live/cam1/master.m3u8?jwt=t",
		"/live/cam1/playlist.m3u8?jwt=t",
		"/live/cam1/video/playlist.m3u8?jwt=t",
		"/live/cam1/video/e000001-g00000000.m4s?jwt=t",
		"/live/cam1/manifest.mpd?jwt=t",
		"/playlist.m3u8?jwt=t",
	}
	tests := map[string]struct {
		err        error
		wantStatus int
	}{
		"no credential":       {err: errNoViewerCredential, wantStatus: http.StatusUnauthorized},
		"an invalid one":      {err: auth.RefusedError{Status: http.StatusUnauthorized}, wantStatus: http.StatusUnauthorized},
		"one for elsewhere":   {err: refused(http.StatusForbidden, "no"), wantStatus: http.StatusForbidden},
		"keys not loaded yet": {err: errors.New("auth: no key set loaded yet"), wantStatus: http.StatusServiceUnavailable},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			check := &fakeViewerCheck{err: tt.err}
			mux := newViewerMux(t, "jwt", check)

			for _, target := range targets {
				rec := serve(mux, target)

				assert.Equal(t, tt.wantStatus, rec.Code, target)
				assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"), target)
				assert.Empty(t, rec.Header().Get("X-Served-Path"), "%s: never reaches the track", target)
				assert.Empty(t, rec.Header().Get("Location"), "%s: a refused redirect names nothing", target)
			}
			for _, ask := range check.questions() {
				assert.Equal(t, viewerAsk{credential: "t", path: "/live/cam1"}, ask, "the token from the query, for the broadcast")
			}
		})
	}
}

// A request for no live broadcast is refused like one for a broadcast the
// viewer may not watch, so refusals don't reveal which broadcasts are live.
func TestStreamMux_ViewerAccess_UnknownBroadcast(t *testing.T) {
	tests := map[string]struct {
		err        error
		wantStatus int
	}{
		"no credential": {err: errNoViewerCredential, wantStatus: http.StatusUnauthorized},
		"refused":       {err: refused(http.StatusForbidden, "no"), wantStatus: http.StatusForbidden},
		"admitted":      {wantStatus: http.StatusNotFound},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			check := &fakeViewerCheck{err: tt.err}
			mux := newViewerMux(t, "jwt", check)

			rec := serve(mux, "/live/cam2/master.m3u8?jwt=t")

			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.Equal(t, []viewerAsk{{credential: "t", path: "/live/cam2"}}, check.questions())
		})
	}
}

func TestStreamMux_ViewerAccess_Admitted(t *testing.T) {
	check := &fakeViewerCheck{}
	mux := newViewerMux(t, "jwt", check)

	master := serve(mux, "/live/cam1/master.m3u8?jwt=t")
	segment := serve(mux, "/live/cam1/video/e000001-g00000000.m4s?jwt=t")
	dash := serve(mux, "/live/cam1/manifest.mpd?jwt=t")
	root := serve(mux, "/playlist.m3u8?jwt=t")

	assert.Equal(t, http.StatusOK, master.Code)
	assert.Equal(t, "private", master.Header().Get("Cache-Control"), "no shared cache serves it to another viewer")
	assert.Contains(t, master.Body.String(), "video/playlist.m3u8?jwt={$jwt}")
	assert.Equal(t, "/live/cam1/video/e000001-g00000000.m4s", segment.Header().Get("X-Served-Path"))
	assert.Equal(t, "/live/cam1/video/manifest.mpd?jwt=t", dash.Header().Get("Location"), "a redirect keeps the token")
	assert.Equal(t, "/live/cam1/master.m3u8?jwt=t", root.Header().Get("Location"))
}

// Without a check nothing changes, and a token parameter alone, for a CDN
// that checks its own tokens, is carried but not checked.
func TestStreamMux_ViewerAccess_Off(t *testing.T) {
	tests := map[string]struct {
		param      string
		wantMaster string
	}{
		"nothing":           {wantMaster: "video/playlist.m3u8\n"},
		"a parameter alone": {param: "token", wantMaster: "video/playlist.m3u8?token={$token}\n"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			mux := newViewerMux(t, tt.param, nil)

			master := serve(mux, "/live/cam1/master.m3u8")

			assert.Equal(t, http.StatusOK, master.Code)
			assert.Empty(t, master.Header().Get("Cache-Control"))
			assert.Contains(t, master.Body.String(), tt.wantMaster)
		})
	}
}

func TestRenderMasterPlaylist_Param(t *testing.T) {
	tracks := []trackInfo{
		{Name: "video", Role: "video", Width: 1280, Height: 720},
		{Name: "audio", Role: "audio"},
	}

	data := string(renderMasterPlaylist(tracks, "jwt"))

	assert.Contains(t, data, "#EXTM3U\n#EXT-X-VERSION:11\n#EXT-X-DEFINE:QUERYPARAM=\"jwt\"\n")
	assert.Contains(t, data, `URI="audio/playlist.m3u8?jwt={$jwt}"`)
	assert.Contains(t, data, "\nvideo/playlist.m3u8?jwt={$jwt}\n")
}

// newViewerVerifier returns a Verifier trusting a fresh key, and the key.
func newViewerVerifier(tb testing.TB) (*auth.Verifier, token.SigningKey) {
	tb.Helper()
	key, err := token.GenerateKey("")
	require.NoError(tb, err)
	set, err := token.MarshalKeySet(key.Public())
	require.NoError(tb, err)
	keys := filepath.Join(tb.TempDir(), "keys.json")
	require.NoError(tb, os.WriteFile(keys, set, 0o600))
	v, err := auth.NewVerifier(auth.VerifierConfig{Keys: keys})
	require.NoError(tb, err)
	return v, key
}

func TestSubscribeCheck(t *testing.T) {
	v, key := newViewerVerifier(t)
	sign := func(g token.Grant) string {
		credential, err := token.Sign(key, g, time.Minute)
		require.NoError(t, err)
		return credential
	}
	tests := map[string]struct {
		credential string
		path       moqt.BroadcastPath
		wantErr    error
		wantStatus int
	}{
		"may watch":               {credential: sign(token.Grant{Root: "/live/", Subscribe: []string{"/cam1"}}), path: "/live/cam1"},
		"may watch the directory": {credential: sign(token.Grant{Root: "/live/", Subscribe: []string{"/"}}), path: "/live/cam1"},
		"no credential":           {path: "/live/cam1", wantErr: errNoViewerCredential},
		"not a token":             {credential: "x", path: "/live/cam1", wantStatus: http.StatusUnauthorized},
		"another broadcast":       {credential: sign(token.Grant{Root: "/live/", Subscribe: []string{"/cam2"}}), path: "/live/cam1", wantStatus: http.StatusForbidden},
		"publish only":            {credential: sign(token.Grant{Root: "/live/", Publish: []string{"/cam1"}}), path: "/live/cam1", wantStatus: http.StatusForbidden},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := subscribeCheck(v)(tt.credential, tt.path)

			switch {
			case tt.wantErr != nil:
				assert.ErrorIs(t, err, tt.wantErr)
			case tt.wantStatus != 0:
				assert.Equal(t, tt.wantStatus, auth.RefusalStatus(err), "%v", err)
			default:
				assert.NoError(t, err)
			}
		})
	}
}

func TestViewerAccessFromEnv(t *testing.T) {
	_, key := newViewerVerifier(t)
	set, err := token.MarshalKeySet(key.Public())
	require.NoError(t, err)
	keys := filepath.Join(t.TempDir(), "keys.json")
	require.NoError(t, os.WriteFile(keys, set, 0o600))
	tests := map[string]struct {
		keys, param string
		wantParam   string
		wantCheck   bool
		wantErr     bool
	}{
		"off":                       {},
		"a parameter for a CDN":     {param: "cdn-token_2", wantParam: "cdn-token_2"},
		"a key set":                 {keys: keys, wantParam: "jwt", wantCheck: true},
		"a key set and a parameter": {keys: keys, param: "token", wantParam: "token", wantCheck: true},
		// The manifests splice the name into every URI they name.
		"a character it bars":  {param: "jwt=", wantErr: true},
		"a space":              {keys: keys, param: "my token", wantErr: true},
		"a substitution brace": {param: "{$jwt}", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("QUMO_AUTH_KEYS", tt.keys)
			t.Setenv("HLS_TOKEN_PARAM", tt.param)

			access, _, err := viewerAccessFromEnv(t.Context())

			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantParam, access.param)
			assert.Equal(t, tt.wantCheck, access.check != nil)
		})
	}
}
