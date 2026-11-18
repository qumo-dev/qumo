package optin

import (
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"

	"github.com/qumo-dev/qumo/token"
)

// An opt-in's broadcast is its path with the directory stripped, cleaned of
// doubled and trailing slashes; one with a "." or ".." segment names none, as
// cleaning it could climb out of the directory the relay granted it in.
func TestBroadcast(t *testing.T) {
	tests := map[string]struct {
		optIn, dir string
		want       string
		wantOK     bool
	}{
		"HLS, directory with a trailing slash":    {optIn: "/.hls/live/cam1", dir: "/.hls/", want: "/live/cam1", wantOK: true},
		"HLS, directory without a trailing slash": {optIn: "/.hls/live/cam1", dir: "/.hls", want: "/live/cam1", wantOK: true},
		"ABR":                               {optIn: "/.abr/live/cam1", dir: token.ABRDirectory, want: "/live/cam1", wantOK: true},
		"a single segment":                  {optIn: "/.hls/cam", dir: "/.hls/", want: "/cam", wantOK: true},
		"a broadcast named like hls":        {optIn: "/.hls/hls/live", dir: "/.hls/", want: "/hls/live", wantOK: true},
		"another directory":                 {optIn: "/custom/stream/room1", dir: "/custom/stream/", want: "/room1", wantOK: true},
		"the bare directory":                {optIn: "/.hls", dir: "/.hls/", want: "/", wantOK: true},
		"a trailing slash":                  {optIn: "/.hls/live/cam1/", dir: "/.hls/", want: "/live/cam1", wantOK: true},
		"a doubled slash":                   {optIn: "/.hls/live//cam1", dir: "/.hls/", want: "/live/cam1", wantOK: true},
		"a name with dots":                  {optIn: "/.hls/live/cam1..hd", dir: "/.hls/", want: "/live/cam1..hd", wantOK: true},
		"a . segment":                       {optIn: "/.abr/live/./cam1/", dir: "/.abr/", wantOK: false},
		"climbing into another's directory": {optIn: "/.hls/acme/../victim/cam", dir: "/.hls/", wantOK: false},
		"climbing out of the directory":     {optIn: "/.hls/../victim/cam", dir: "/.hls/", wantOK: false},
		"a trailing ..":                     {optIn: "/.hls/acme/..", dir: "/.hls/", wantOK: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := Broadcast(tt.optIn, tt.dir)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestOf(t *testing.T) {
	tests := map[moqt.BroadcastPath]struct {
		want   token.OptIn
		wantOK bool
	}{
		"/.abr/live/cam1": {want: token.OptInABR, wantOK: true},
		"/.hls/live/cam1": {want: token.OptInHLS, wantOK: true},
		"/live/cam1":      {},
		"/.abrx/cam1":     {},
		"/live/.abr/cam1": {},
	}
	for p, tt := range tests {
		got, ok := Of(p)
		assert.Equal(t, tt.wantOK, ok, p)
		assert.Equal(t, tt.want, got, p)
	}
}

func TestHasDotSegment(t *testing.T) {
	tests := map[string]struct {
		path string
		want bool
	}{
		"plain":          {path: "/acme/cam", want: false},
		"dots in a name": {path: "/acme/cam..hd", want: false},
		"a . segment":    {path: "/acme/./cam", want: true},
		"a .. segment":   {path: "/acme/../cam", want: true},
		"a trailing ..":  {path: "/acme/..", want: true},
		"empty segments": {path: "/acme//cam/", want: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, HasDotSegment(tt.path))
		})
	}
}

func TestPath(t *testing.T) {
	assert.Equal(t, moqt.BroadcastPath("/.abr/live/cam1"), Path(token.ABRDirectory, "/live/cam1"))
	assert.Equal(t, moqt.BroadcastPath("/.hls/live/cam1"), Path("/.hls", "/live/cam1"))
	assert.Equal(t, moqt.BroadcastPath("/.abr/cam"), Path(token.ABRDirectory, "cam"))
}

func TestPathRoundTrip(t *testing.T) {
	for _, dir := range []string{token.HLSDirectory, token.ABRDirectory} {
		for _, b := range []string{"/live/cam1", "/a", "/room/42/cam"} {
			got, ok := Broadcast(string(Path(dir, moqt.BroadcastPath(b))), dir)
			assert.True(t, ok, "%s %s", dir, b)
			assert.Equal(t, b, got, "%s %s", dir, b)
		}
	}
}
