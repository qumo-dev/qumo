package optin

import (
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"

	"github.com/qumo-dev/qumo/token"
)

func TestBroadcast(t *testing.T) {
	tests := map[string]struct {
		optIn, dir, want string
	}{
		"HLS, directory with a trailing slash":    {optIn: "/.hls/live/cam1", dir: "/.hls/", want: "/live/cam1"},
		"HLS, directory without a trailing slash": {optIn: "/.hls/live/cam1", dir: "/.hls", want: "/live/cam1"},
		"ABR":                         {optIn: "/.abr/live/cam1", dir: token.ABRDirectory, want: "/live/cam1"},
		"a single segment":            {optIn: "/.hls/cam", dir: "/.hls/", want: "/cam"},
		"a broadcast named like hls":  {optIn: "/.hls/hls/live", dir: "/.hls/", want: "/hls/live"},
		"another directory":           {optIn: "/custom/stream/room1", dir: "/custom/stream/", want: "/room1"},
		"the bare directory":          {optIn: "/.hls", dir: "/.hls/", want: "/"},
		"dot segments are cleaned up": {optIn: "/.abr/live/./cam1/", dir: "/.abr/", want: "/live/cam1"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, Broadcast(tt.optIn, tt.dir))
		})
	}
}

func TestPath(t *testing.T) {
	assert.Equal(t, moqt.BroadcastPath("/.abr/live/cam1"), Path(token.ABRDirectory, "/live/cam1"))
	assert.Equal(t, moqt.BroadcastPath("/.hls/live/cam1"), Path("/.hls", "/live/cam1"))
	assert.Equal(t, moqt.BroadcastPath("/.abr/cam"), Path(token.ABRDirectory, "cam"))
}

func TestPathRoundTrip(t *testing.T) {
	for _, dir := range token.OptInDirectories() {
		for _, b := range []string{"/live/cam1", "/a", "/room/42/cam"} {
			assert.Equal(t, b, Broadcast(string(Path(dir, moqt.BroadcastPath(b))), dir), "%s %s", dir, b)
		}
	}
}
