package hls

import (
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// broadcastPathFromAnnounce strips the announcement prefix from annPath to find
// the target broadcast path where the publisher uploads media.
//
// For example, with prefix "/.hls/":
//
//	"/.hls/live/cam1" -> "/live/cam1"
//	"/.hls/cam"       -> "/cam"
//	"/.hls"           -> "/"
func broadcastPathFromAnnounce(annPath, prefix string) string {
	cleanPrefix := strings.TrimSuffix(prefix, "/")
	p := strings.TrimPrefix(annPath, cleanPrefix)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

// trackInfo holds metadata describing a media track in an HLS master playlist.
type trackInfo struct {
	Name    string
	Role    string // "video", "audio"
	Codec   string
	Width   uint16
	Height  uint16
	Bitrate int64
}

// trackEntry is a mounted media track under a broadcast.
type trackEntry struct {
	info    trackInfo
	handler http.Handler
	live    *liveness
}

// broadcastEntry is the concurrent registry of media tracks for one broadcast.
type broadcastEntry struct {
	path   string
	mu     sync.RWMutex
	tracks map[string]*trackEntry
}

func newBroadcastEntry(p string) *broadcastEntry {
	return &broadcastEntry{
		path:   p,
		tracks: make(map[string]*trackEntry),
	}
}

// AddTrack registers or updates a track under this broadcast.
func (b *broadcastEntry) AddTrack(t trackEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tracks[t.info.Name] = &t
}

// RemoveTrack removes a track from this broadcast.
func (b *broadcastEntry) RemoveTrack(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.tracks, name)
}

func (b *broadcastEntry) getTrack(name string) (*trackEntry, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	t, ok := b.tracks[name]
	return t, ok
}

// trackInfos returns a snapshot of metadata for all tracks in this broadcast.
func (b *broadcastEntry) trackInfos() []trackInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()
	infos := make([]trackInfo, 0, len(b.tracks))
	for _, t := range b.tracks {
		infos = append(infos, t.info)
	}
	return infos
}

// defaultTrack returns the primary video track, or the first available track.
func (b *broadcastEntry) defaultTrack() (*trackEntry, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if t, ok := b.tracks["video"]; ok {
		return t, true
	}
	for _, t := range b.tracks {
		return t, true
	}
	return nil, false
}

// isLive reports whether at least one track in the broadcast is currently live.
func (b *broadcastEntry) isLive(now time.Time, timeout time.Duration) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.tracks) == 0 {
		return false
	}
	for _, t := range b.tracks {
		if t.live != nil && !t.live.stale(now, timeout) {
			return true
		}
	}
	return false
}

// streamMux dynamically routes HLS and DASH requests to active broadcasts and
// tracks:
//
//	/{broadcast}/master.m3u8          - HLS master playlist
//	/{broadcast}/playlist.m3u8        - HLS master playlist (convenience alias)
//	/{broadcast}/{track}/playlist.m3u8 - HLS media playlist for {track}
//	/{broadcast}/{track}/manifest.mpd - DASH MPD for {track}
//	/{broadcast}/{track}/init.m4s     - fMP4 init segment for {track}
//	/{broadcast}/{track}/<group>.m4s  - media segment for {track}
//	/{broadcast}/manifest.mpd         - DASH MPD for default track
type streamMux struct {
	mu          sync.RWMutex
	broadcasts  map[string]*broadcastEntry
	fallback    http.Handler
	liveTimeout time.Duration
}

func newStreamMux(liveTimeout time.Duration, fallback http.Handler) *streamMux {
	return &streamMux{
		broadcasts:  make(map[string]*broadcastEntry),
		fallback:    fallback,
		liveTimeout: liveTimeout,
	}
}

// RegisterBroadcast registers or retrieves a broadcast entry.
func (m *streamMux) RegisterBroadcast(p string) *broadcastEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	clean := path.Clean(p)
	b, ok := m.broadcasts[clean]
	if !ok {
		b = newBroadcastEntry(clean)
		m.broadcasts[clean] = b
	}
	return b
}

// UnregisterBroadcast unregisters an active broadcast.
func (m *streamMux) UnregisterBroadcast(p string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	clean := path.Clean(p)
	delete(m.broadcasts, clean)
}

func (m *streamMux) getBroadcast(p string) (*broadcastEntry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	clean := path.Clean(p)
	b, ok := m.broadcasts[clean]
	return b, ok
}

// activeBroadcastCount returns the number of active broadcasts.
func (m *streamMux) activeBroadcastCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.broadcasts)
}

// singleBroadcast returns the only registered broadcast if exactly one exists.
func (m *streamMux) singleBroadcast() *broadcastEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.broadcasts) == 1 {
		for _, b := range m.broadcasts {
			return b
		}
	}
	return nil
}

func (m *streamMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cleanPath := path.Clean(r.URL.Path)
	base := path.Base(cleanPath)

	// 1. Explicit master playlist: /{broadcast}/master.m3u8
	if base == "master.m3u8" {
		broadcastPath := path.Dir(cleanPath)
		if b, ok := m.getBroadcast(broadcastPath); ok {
			m.serveMasterPlaylist(w, r, b)
			return
		}
	}

	// 2. Track route: /{broadcast}/{track}/*
	dir := path.Dir(cleanPath)
	broadcastPath := path.Dir(dir)
	trackName := path.Base(dir)
	if b, ok := m.getBroadcast(broadcastPath); ok {
		if t, ok := b.getTrack(trackName); ok {
			t.handler.ServeHTTP(w, r)
			return
		}
	}

	// 3. Broadcast-level playlist: /{broadcast}/playlist.m3u8
	if base == "playlist.m3u8" {
		broadcastPath := path.Dir(cleanPath)
		if b, ok := m.getBroadcast(broadcastPath); ok {
			m.serveMasterPlaylist(w, r, b)
			return
		}
	}

	// 4. Broadcast-level DASH MPD: /{broadcast}/manifest.mpd
	if base == "manifest.mpd" {
		broadcastPath := path.Dir(cleanPath)
		if b, ok := m.getBroadcast(broadcastPath); ok {
			if t, ok := b.defaultTrack(); ok {
				t.handler.ServeHTTP(w, r)
				return
			}
		}
	}

	// 5. Root fallback for single broadcast or configured fallback handler
	if m.fallback != nil {
		m.fallback.ServeHTTP(w, r)
		return
	}

	// When exactly one broadcast is active, root requests alias to it.
	if (base == "playlist.m3u8" || base == "master.m3u8") && cleanPath == "/"+base {
		if single := m.singleBroadcast(); single != nil {
			m.serveMasterPlaylist(w, r, single)
			return
		}
	}

	http.NotFound(w, r)
}

func (m *streamMux) serveMasterPlaylist(w http.ResponseWriter, _ *http.Request, b *broadcastEntry) {
	if !b.isLive(time.Now(), m.liveTimeout) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "hls: no media is currently arriving for this broadcast",
			http.StatusServiceUnavailable)
		return
	}

	tracks := b.trackInfos()
	if len(tracks) == 0 {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "hls: no media tracks available for this broadcast",
			http.StatusServiceUnavailable)
		return
	}

	body := renderMasterPlaylist(tracks)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	_, _ = w.Write(body)
}

// renderMasterPlaylist formats an RFC 8216 HLS master playlist from media track
// metadata.
func renderMasterPlaylist(tracks []trackInfo) []byte {
	sorted := make([]trackInfo, len(tracks))
	copy(sorted, tracks)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Name < sorted[j].Name
	})

	var audioTracks []trackInfo
	var videoTracks []trackInfo
	for _, t := range sorted {
		if t.Role == "audio" {
			audioTracks = append(audioTracks, t)
		} else {
			videoTracks = append(videoTracks, t)
		}
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:6\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")

	hasAudio := len(audioTracks) > 0
	if hasAudio {
		for i, a := range audioTracks {
			def := "NO"
			if i == 0 {
				def = "YES"
			}
			fmt.Fprintf(&b, "#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=%q,DEFAULT=%s,AUTOSELECT=YES,URI=%q\n",
				a.Name, def, a.Name+"/playlist.m3u8")
		}
	}

	if len(videoTracks) > 0 {
		for _, v := range videoTracks {
			bw := v.Bitrate
			if bw <= 0 {
				if v.Width > 0 && v.Height > 0 {
					bw = int64(v.Width) * int64(v.Height) * 2
				} else {
					bw = 2_000_000
				}
			}
			var attrs []string
			attrs = append(attrs, fmt.Sprintf("BANDWIDTH=%d", bw))
			if v.Width > 0 && v.Height > 0 {
				attrs = append(attrs, fmt.Sprintf("RESOLUTION=%dx%d", v.Width, v.Height))
			}
			if v.Codec != "" {
				attrs = append(attrs, fmt.Sprintf("CODECS=%q", v.Codec))
			}
			if hasAudio {
				attrs = append(attrs, "AUDIO=\"audio\"")
			}
			fmt.Fprintf(&b, "#EXT-X-STREAM-INF:%s\n%s/playlist.m3u8\n",
				strings.Join(attrs, ","), v.Name)
		}
	} else if hasAudio {
		for _, a := range audioTracks {
			bw := a.Bitrate
			if bw <= 0 {
				bw = 128_000
			}
			var attrs []string
			attrs = append(attrs, fmt.Sprintf("BANDWIDTH=%d", bw))
			if a.Codec != "" {
				attrs = append(attrs, fmt.Sprintf("CODECS=%q", a.Codec))
			}
			fmt.Fprintf(&b, "#EXT-X-STREAM-INF:%s\n%s/playlist.m3u8\n",
				strings.Join(attrs, ","), a.Name)
		}
	}

	return []byte(b.String())
}
