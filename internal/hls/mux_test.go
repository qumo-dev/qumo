package hls

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_broadcastPathFromAnnounce(t *testing.T) {
	tests := []struct {
		name     string
		annPath  string
		prefix   string
		expected string
	}{
		{
			name:     "standard .hls prefix with trailing slash",
			annPath:  "/.hls/live/cam1",
			prefix:   "/.hls/",
			expected: "/live/cam1",
		},
		{
			name:     "standard .hls prefix without trailing slash",
			annPath:  "/.hls/live/cam1",
			prefix:   "/.hls",
			expected: "/live/cam1",
		},
		{
			name:     "single segment path",
			annPath:  "/.hls/cam",
			prefix:   "/.hls/",
			expected: "/cam",
		},
		{
			name:     "hls/live under .hls prefix",
			annPath:  "/.hls/hls/live",
			prefix:   "/.hls/",
			expected: "/hls/live",
		},
		{
			name:     "custom prefix",
			annPath:  "/custom/stream/room1",
			prefix:   "/custom/stream/",
			expected: "/room1",
		},
		{
			name:     "bare prefix",
			annPath:  "/.hls",
			prefix:   "/.hls/",
			expected: "/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := broadcastPathFromAnnounce(tt.annPath, tt.prefix)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func Test_renderMasterPlaylist(t *testing.T) {
	t.Run("single video track", func(t *testing.T) {
		tracks := []trackInfo{
			{
				Name:    "video",
				Role:    "video",
				Codec:   "avc1.42E01E",
				Width:   1920,
				Height:  1080,
				Bitrate: 4_500_000,
			},
		}
		data := string(renderMasterPlaylist(tracks))
		assert.Contains(t, data, "#EXTM3U")
		assert.Contains(t, data, "#EXT-X-VERSION:6")
		assert.Contains(t, data, "#EXT-X-INDEPENDENT-SEGMENTS")
		assert.Contains(t, data, `BANDWIDTH=4500000,RESOLUTION=1920x1080,CODECS="avc1.42E01E"`)
		assert.Contains(t, data, "video/playlist.m3u8")
	})

	t.Run("multi-variant video tracks", func(t *testing.T) {
		tracks := []trackInfo{
			{
				Name:    "video-low",
				Role:    "video",
				Codec:   "avc1.42E01E",
				Width:   640,
				Height:  360,
				Bitrate: 800_000,
			},
			{
				Name:    "video-high",
				Role:    "video",
				Codec:   "avc1.64001F",
				Width:   1920,
				Height:  1080,
				Bitrate: 5_000_000,
			},
		}
		data := string(renderMasterPlaylist(tracks))
		assert.Contains(t, data, "video-high/playlist.m3u8")
		assert.Contains(t, data, "video-low/playlist.m3u8")
		assert.Contains(t, data, "BANDWIDTH=5000000")
		assert.Contains(t, data, "BANDWIDTH=800000")
	})

	t.Run("video with audio", func(t *testing.T) {
		tracks := []trackInfo{
			{
				Name:    "video",
				Role:    "video",
				Codec:   "avc1.42E01E",
				Width:   1280,
				Height:  720,
				Bitrate: 2_500_000,
			},
			{
				Name:    "audio",
				Role:    "audio",
				Codec:   "mp4a.40.2",
				Bitrate: 128_000,
			},
		}
		data := string(renderMasterPlaylist(tracks))
		assert.Contains(t, data, `#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="audio",DEFAULT=YES,AUTOSELECT=YES,URI="audio/playlist.m3u8"`)
		assert.Contains(t, data, `AUDIO="audio"`)
		assert.Contains(t, data, "video/playlist.m3u8")
	})
}

func Test_streamMux_Routing(t *testing.T) {
	mux := newStreamMux(10 * time.Second)
	b := mux.RegisterBroadcast("/live/cam1")

	liveVideo := &liveness{}
	liveVideo.mark(time.Now())

	videoHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("video-track-ok:" + r.URL.Path))
	})

	b.AddTrack(trackEntry{
		info: trackInfo{
			Name:    "video",
			Role:    "video",
			Codec:   "avc1.42E01E",
			Width:   1280,
			Height:  720,
			Bitrate: 2_000_000,
		},
		handler: videoHandler,
		live:    liveVideo,
	})

	// 1. Master playlist explicit
	req := httptest.NewRequest(http.MethodGet, "/live/cam1/master.m3u8", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/vnd.apple.mpegurl", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Body.String(), "video/playlist.m3u8")

	// 2. Master playlist alias: /live/cam1/playlist.m3u8
	req = httptest.NewRequest(http.MethodGet, "/live/cam1/playlist.m3u8", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "video/playlist.m3u8")

	// 3. Track media playlist: /live/cam1/video/playlist.m3u8
	req = httptest.NewRequest(http.MethodGet, "/live/cam1/video/playlist.m3u8", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "video-track-ok:/live/cam1/video/playlist.m3u8", rec.Body.String())

	// 4. Track segment: /live/cam1/video/e000000-g00000000.m4s
	req = httptest.NewRequest(http.MethodGet, "/live/cam1/video/e000000-g00000000.m4s", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "video-track-ok:/live/cam1/video/e000000-g00000000.m4s", rec.Body.String())

	// 5. Root alias when single broadcast is active: redirected, so the master
	// playlist's relative variant URIs resolve beneath the broadcast.
	req = httptest.NewRequest(http.MethodGet, "/playlist.m3u8", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "/live/cam1/master.m3u8", rec.Header().Get("Location"))

	// 5b. Broadcast-level DASH MPD: redirected to the default track's MPD, whose
	// relative init and segment URIs resolve beneath the track.
	req = httptest.NewRequest(http.MethodGet, "/live/cam1/manifest.mpd", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "/live/cam1/video/manifest.mpd", rec.Header().Get("Location"))

	// 6. Unknown broadcast -> 404
	req = httptest.NewRequest(http.MethodGet, "/nonexistent/master.m3u8", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// 7. Stale broadcast -> 503
	liveVideo.mu.Lock()
	liveVideo.last = time.Time{} // never arrived
	liveVideo.mu.Unlock()

	req = httptest.NewRequest(http.MethodGet, "/live/cam1/master.m3u8", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// An ending announcement unmounts only its own entry, not the one a newer
// announcement of the same broadcast mounted since.
func Test_streamMux_UnregisterBroadcast(t *testing.T) {
	t.Run("own entry is removed", func(t *testing.T) {
		mux := newStreamMux(10 * time.Second)
		b := mux.RegisterBroadcast("/live/cam1")
		mux.UnregisterBroadcast(b)
		_, ok := mux.getBroadcast("/live/cam1")
		assert.False(t, ok)
	})

	t.Run("successor's entry survives", func(t *testing.T) {
		mux := newStreamMux(10 * time.Second)
		older := mux.RegisterBroadcast("/live/cam1")
		newer := mux.RegisterBroadcast("/live/cam1")
		mux.UnregisterBroadcast(older)
		got, ok := mux.getBroadcast("/live/cam1")
		require.True(t, ok)
		assert.Same(t, newer, got)
	})
}
