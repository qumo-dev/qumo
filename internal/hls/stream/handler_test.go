package stream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/okdaichi/qumo-ledger/ledger"
	"github.com/okdaichi/qumo-ledger/ledger/store"
	"github.com/okdaichi/qumo-ledger/ledger/store/mem"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testInit satisfies NewHandler's init requirement for the fmp4 fixture. Tests
// whose subject is not initialization pass it so they exercise the path they are
// actually about; see TestNewHandler_InitRequired for the guard itself.
var testInit = InitSegment{Bytes: []byte("init-bytes")}

// fixtureEpoch is when the fixture's presentation starts on the wall clock.
var fixtureEpoch = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

// trackFixture is a track with two committed segments, plus the metadata and
// payloads a serving test wants to assert against.
type trackFixture struct {
	track   *ledger.Track
	metas   []ledger.GroupInfo
	payload [][]byte
}

func newTrackFixture(tb testing.TB) trackFixture {
	tb.Helper()
	return newTrackFixtureEncoding(tb, "fmp4")
}

func newTrackFixtureEncoding(tb testing.TB, encoding string) trackFixture {
	tb.Helper()
	return newTrackFixtureGroups(tb, encoding, 2)
}

func newTrackFixtureGroups(tb testing.TB, encoding string, groups int64) trackFixture {
	tb.Helper()
	return newTrackFixtureStore(tb, encoding, groups, mem.New())
}

// newTrackFixtureStore is newTrackFixtureGroups over a caller-supplied backend,
// for tests that need to instrument the store mid-test.
func newTrackFixtureStore(tb testing.TB, encoding string, groups int64, backend store.Store) trackFixture {
	tb.Helper()
	ctx := context.Background()

	track, err := ledger.Create(ctx, backend, "live/cam1/video", ledger.TrackSchema{
		Timescale: 90000, TimeSource: ledger.TimeSourceFrame,
		MIME: "video/mp4", Encoding: encoding,
	}, ledger.Config{})
	require.NoError(tb, err)

	writer, err := track.Writer(ctx)
	require.NoError(tb, err)

	fix := trackFixture{track: track}
	for sequence := range groups {
		payload := []byte("frames-" + string(rune('A'+sequence)))
		meta, err := writer.AppendGroup(ctx, ledger.GroupInfo{
			ID:        ledger.NewGroupID(0, uint64(sequence)),
			MediaTime: sequence * 180000,
			Duration:  180000,
			// Anchored on a fixed clock advancing with media time, so the
			// presentation start these imply is the same for every group — which
			// is what a stable DASH availabilityStartTime depends on.
			Wallclock: fixtureEpoch.Add(time.Duration(sequence) * 2 * time.Second).UnixNano(),
		}, payload)
		require.NoError(tb, err)
		fix.metas = append(fix.metas, meta)
		fix.payload = append(fix.payload, payload)
	}
	return fix
}

func TestHandler_ServeHTTP_HLSPlaylist(t *testing.T) {
	fix := newTrackFixture(t)
	handler, err := NewHandler(fix.track, Options{InitSegment: testInit})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/playlist.m3u8")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/vnd.apple.mpegurl", resp.Header.Get("Content-Type"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	got := string(body)

	assert.Contains(t, got, "#EXT-X-PLAYLIST-TYPE:EVENT")
	assert.Contains(t, got, fix.metas[0].ID.String()+".m4s", "the playlist points at segments by GroupID")
	assert.Contains(t, got, fix.metas[1].ID.String()+".m4s")
}

func TestHandler_ServeHTTP_DASHManifest(t *testing.T) {
	fix := newTrackFixture(t)
	handler, err := NewHandler(fix.track, Options{InitSegment: testInit})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/manifest.mpd")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/dash+xml", resp.Header.Get("Content-Type"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	got := string(body)

	assert.Contains(t, got, `type="dynamic"`)
	assert.Contains(t, got, `<SegmentURL media="`+fix.metas[0].ID.String()+`.m4s"/>`,
		"the MPD points at segments by GroupID, the same URLs as the HLS playlist")
}

// The default resolver proxies: a segment request returns the stored bytes.
func TestHandler_ServeHTTP_SegmentProxies(t *testing.T) {
	fix := newTrackFixture(t)
	handler, err := NewHandler(fix.track, Options{InitSegment: testInit})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/" + fix.metas[0].ID.String() + ".m4s")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "video/mp4", resp.Header.Get("Content-Type"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, fix.payload[0], body, "the proxy serves the exact bytes that were appended")
}

// A redirect resolver sends the player straight to the object: the handler asks
// the resolver and 302s to whatever URL it mints from ObjectKey.
func TestHandler_ServeHTTP_SegmentRedirect(t *testing.T) {
	fix := newTrackFixture(t)
	handler, err := NewHandler(fix.track, Options{
		InitSegment: testInit,
		Resolver: RedirectResolver(func(g ledger.GroupInfo) string {
			return "https://objects.example/" + g.ObjectKey
		}),
	})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Do not follow the redirect: the test asserts the 302, not the object store.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(ts.URL + "/" + fix.metas[1].ID.String() + ".m4s")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusFound, resp.StatusCode)
	loc, err := resp.Location()
	require.NoError(t, err)
	assert.Equal(t, "https://objects.example/"+fix.metas[1].ObjectKey, loc.String())
}

func TestHandler_ServeHTTP_UnknownSegmentNotFound(t *testing.T) {
	fix := newTrackFixture(t)
	handler, err := NewHandler(fix.track, Options{InitSegment: testInit})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// A sequence that was never committed.
	resp, err := http.Get(ts.URL + "/e000001-g00000099.m4s")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// Serving is concurrent-safe: many segment requests at once must not race. The
// race detector is the assertion.
func TestHandler_ServeHTTP_ConcurrentSegments(t *testing.T) {
	fix := newTrackFixture(t)
	handler, err := NewHandler(fix.track, Options{InitSegment: testInit})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			resp, err := http.Get(ts.URL + "/" + fix.metas[0].ID.String() + ".m4s")
			if !assert.NoError(t, err) {
				return
			}
			defer resp.Body.Close()
			assert.Equal(t, http.StatusOK, resp.StatusCode)
		})
	}
	wg.Wait()
}

// A fragmented-MP4 track cannot be played without an init segment, so building a
// handler without one fails rather than serving manifests that omit #EXT-X-MAP
// (and DASH @initialization) with a 200.
func TestNewHandler_InitRequired(t *testing.T) {
	tests := map[string]struct {
		encoding string
		init     InitSegment
		wantErr  bool
	}{
		"fmp4 without init":    {encoding: "fmp4", wantErr: true},
		"fmp4 with init bytes": {encoding: "fmp4", init: InitSegment{Bytes: []byte("init-bytes")}},
		// A URL satisfies the requirement: the segment exists, this handler just
		// does not serve its bytes.
		"fmp4 with init url": {encoding: "fmp4", init: InitSegment{URL: "https://objects.example/init.m4s"}},
		// MPEG-TS repeats its PAT/PMT in every segment, so it needs no init.
		"mpegts without init": {encoding: "ts"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fix := newTrackFixtureEncoding(t, tt.encoding)

			handler, err := NewHandler(fix.track, Options{InitSegment: tt.init})
			if tt.wantErr {
				assert.ErrorIs(t, err, ErrInitRequired)
				assert.Nil(t, handler)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, handler)
		})
	}
}

func TestNewHandler_NilTrack(t *testing.T) {
	handler, err := NewHandler(nil, Options{})

	assert.Error(t, err)
	assert.Nil(t, handler)
}

// A container that carries its codec configuration in every segment has no
// init segment, so neither manifest may point a player at one, and segments are
// addressed by the extension the encoding implies.
func TestHandler_ServeHTTP_ContainerWithoutInit(t *testing.T) {
	fix := newTrackFixtureEncoding(t, "ts")
	handler, err := NewHandler(fix.track, Options{})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	get := func(tb testing.TB, path string) (int, string) {
		tb.Helper()
		resp, err := http.Get(ts.URL + path)
		require.NoError(tb, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(tb, err)
		return resp.StatusCode, string(body)
	}
	segment := fix.metas[0].ID.String() + ".ts"

	status, playlist := get(t, "/playlist.m3u8")
	require.Equal(t, http.StatusOK, status)
	assert.NotContains(t, playlist, "#EXT-X-MAP")
	assert.Contains(t, playlist, segment)

	status, manifest := get(t, "/manifest.mpd")
	require.Equal(t, http.StatusOK, status)
	assert.NotContains(t, manifest, "<Initialization")
	assert.Contains(t, manifest, segment)

	status, body := get(t, "/"+segment)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, string(fix.payload[0]), body)

	// With nothing to serve as an init segment, its name is just another name
	// that is not a group id.
	status, _ = get(t, "/init.ts")
	assert.Equal(t, http.StatusNotFound, status)
}

// A window caps the playlist at the newest segments and turns it into a sliding
// live playlist: EVENT forbids removing segments, and EXT-X-MEDIA-SEQUENCE must
// count the ones that rolled off.
func TestHandler_ServeHTTP_Window(t *testing.T) {
	const total, window = 10, 3
	fix := newTrackFixtureGroups(t, "fmp4", total)

	handler, err := NewHandler(fix.track, Options{
		InitSegment: testInit,
		Window:      window,
	})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/playlist.m3u8")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	got := string(body)

	assert.NotContains(t, got, "#EXT-X-PLAYLIST-TYPE:EVENT",
		"a playlist that drops segments cannot be EVENT")
	assert.Contains(t, got, "#EXT-X-MEDIA-SEQUENCE:7",
		"the first listed segment follows the 7 that rolled off")

	for _, meta := range fix.metas[:total-window] {
		assert.NotContains(t, got, meta.ID.String()+".m4s",
			"segments older than the window are not listed")
	}
	for _, meta := range fix.metas[total-window:] {
		assert.Contains(t, got, meta.ID.String()+".m4s",
			"the newest segments are listed, in commit order")
	}

	// Rolling out of the playlist is not deletion: the ledger keeps every group,
	// so an older segment a client already holds a URL for still resolves.
	old := fix.metas[0]
	oldResp, err := http.Get(ts.URL + "/" + old.ID.String() + ".m4s")
	require.NoError(t, err)
	defer oldResp.Body.Close()
	assert.Equal(t, http.StatusOK, oldResp.StatusCode)
}

// A window larger than the track lists everything and drops nothing.
func TestHandler_ServeHTTP_WindowLargerThanTrack(t *testing.T) {
	fix := newTrackFixtureGroups(t, "fmp4", 2)

	handler, err := NewHandler(fix.track, Options{
		InitSegment: testInit,
		Window:      10,
	})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/playlist.m3u8")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	got := string(body)

	assert.Contains(t, got, "#EXT-X-MEDIA-SEQUENCE:0")
	assert.Contains(t, got, fix.metas[0].ID.String()+".m4s")
	assert.Contains(t, got, fix.metas[1].ID.String()+".m4s")
}

// A window that has rolled past an epoch change must still tell the player the
// timeline reset. The reset belongs to the first listed segment, where nothing
// in the listed groups reveals it, and the resets already gone from the playlist
// have to be counted so discontinuity numbering survives them.
func TestHandler_ServeHTTP_WindowAcrossEpochs(t *testing.T) {
	ctx := context.Background()
	store := mem.New()
	track, err := ledger.Create(ctx, store, "live/cam1/video", ledger.TrackSchema{
		Timescale: 90000, TimeSource: ledger.TimeSourceFrame,
		MIME: "video/mp4", Encoding: "fmp4",
	}, ledger.Config{})
	require.NoError(t, err)

	writer, err := track.Writer(ctx)
	require.NoError(t, err)

	// Three groups, then a producer restart, then three more. Windowing to the
	// last four puts the epoch boundary one segment into the window; windowing
	// to two puts it behind the window entirely.
	appendGroups := func(n int64) {
		for i := range n {
			_, err := writer.AppendGroup(ctx, ledger.GroupInfo{
				ID:        ledger.NewGroupID(0, uint64(i)),
				MediaTime: i * 180000,
				Duration:  180000,
			}, []byte("frames"))
			require.NoError(t, err)
		}
	}
	appendGroups(3)
	require.NoError(t, writer.NewEpoch(ctx))
	appendGroups(3)

	t.Run("boundary inside the window", func(t *testing.T) {
		got := playlistFor(t, track, 4)
		assert.Contains(t, got, "#EXT-X-DISCONTINUITY",
			"the epoch change is listed, so its reset is marked")
		assert.NotContains(t, got, "#EXT-X-DISCONTINUITY-SEQUENCE",
			"no reset has rolled off, and an absent tag already means zero")
	})

	t.Run("window opens on the boundary", func(t *testing.T) {
		// The last three segments are exactly the new epoch, so the reset sits
		// at the first listed segment.
		got := playlistFor(t, track, 3)
		assert.Contains(t, got, "#EXT-X-DISCONTINUITY",
			"a window opening on a new epoch still announces the reset")
	})

	t.Run("boundary behind the window", func(t *testing.T) {
		got := playlistFor(t, track, 2)
		assert.NotContains(t, got, "\n#EXT-X-DISCONTINUITY\n",
			"the reset is no longer between two listed segments")
		assert.Contains(t, got, "#EXT-X-DISCONTINUITY-SEQUENCE:1",
			"but the player is told one reset has rolled off")
	})
}

// A producer that restarts opens a new epoch, and the lifetimes before it have
// ended. EpochWindow caps how many a manifest lists, so a player is not left to
// open the stream on a finished session and watch it through.
func TestHandler_ServeHTTP_EpochWindow(t *testing.T) {
	ctx := context.Background()
	store := mem.New()
	track, err := ledger.Create(ctx, store, "live/cam1/video", ledger.TrackSchema{
		Timescale: 90000, TimeSource: ledger.TimeSourceFrame,
		MIME: "video/mp4", Encoding: "fmp4",
	}, ledger.Config{})
	require.NoError(t, err)

	writer, err := track.Writer(ctx)
	require.NoError(t, err)

	appendGroups := func(n int64) []ledger.GroupInfo {
		var out []ledger.GroupInfo
		for i := range n {
			meta, err := writer.AppendGroup(ctx, ledger.GroupInfo{
				ID:        ledger.NewGroupID(0, uint64(i)),
				MediaTime: i * 180000,
				Duration:  180000,
				Wallclock: fixtureEpoch.Add(time.Duration(i) * 2 * time.Second).UnixNano(),
			}, []byte("frames"))
			require.NoError(t, err)
			out = append(out, meta)
		}
		return out
	}

	// Three lifetimes: two that have ended, then the one currently running.
	oldest := appendGroups(3)
	require.NoError(t, writer.NewEpoch(ctx))
	previous := appendGroups(4)
	require.NoError(t, writer.NewEpoch(ctx))
	live := appendGroups(2)

	ended := append(append([]ledger.GroupInfo{}, oldest...), previous...)

	render := func(opts Options) string {
		opts.InitSegment = testInit
		handler, err := NewHandler(track, opts)
		require.NoError(t, err)
		ts := httptest.NewServer(handler)
		defer ts.Close()

		resp, err := http.Get(ts.URL + "/playlist.m3u8")
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return string(body)
	}

	listed := func(t *testing.T, got string, want []ledger.GroupInfo, unwanted []ledger.GroupInfo) {
		t.Helper()
		for _, meta := range unwanted {
			assert.NotContains(t, got, meta.ID.String()+".m4s",
				"a segment from a lifetime outside the epoch window is not listed")
		}
		for _, meta := range want {
			assert.Contains(t, got, meta.ID.String()+".m4s")
		}
	}

	// Only the current session. The segment window is a separate concern, so
	// this has to hold with and without one — and a window wide enough to reach
	// back into an ended lifetime must not drag it in.
	for name, opts := range map[string]Options{
		"unwindowed":               {EpochWindow: 1},
		"window spans the restart": {EpochWindow: 1, Window: 8},
	} {
		t.Run("one lifetime, "+name, func(t *testing.T) {
			got := render(opts)
			listed(t, got, live, ended)
			assert.Contains(t, got, "#EXT-X-MEDIA-SEQUENCE:7",
				"the seven segments of the two ended lifetimes are behind the manifest")
			assert.Contains(t, got, "#EXT-X-DISCONTINUITY",
				"a client polling across the restart is told the timeline reset")
			// A discontinuity belongs to the segment after it, so only the reset
			// opening the middle lifetime has left; the one opening the listed
			// lifetime is still in the manifest, above its first segment.
			assert.Contains(t, got, "#EXT-X-DISCONTINUITY-SEQUENCE:1")
		})
	}

	// Two keeps the session before the restart, so a viewer already playing it
	// reaches the new one across a discontinuity instead of losing its segments.
	t.Run("two lifetimes", func(t *testing.T) {
		got := render(Options{EpochWindow: 2})
		listed(t, got, append(append([]ledger.GroupInfo{}, previous...), live...), oldest)
		assert.Contains(t, got, "#EXT-X-MEDIA-SEQUENCE:3",
			"only the oldest lifetime is behind the manifest")
		// The oldest lifetime opened the track, so it carried no reset, and both
		// resets that exist still sit above segments the manifest lists.
		assert.NotContains(t, got, "#EXT-X-DISCONTINUITY-SEQUENCE")
		assert.Equal(t, 2, strings.Count(got, "#EXT-X-DISCONTINUITY\n"),
			"each listed lifetime after the first opens with a reset")
	})

	// More lifetimes than exist lists them all, the same as no epoch window.
	t.Run("more than the track has", func(t *testing.T) {
		listed(t, render(Options{EpochWindow: 9}), append(ended, live...), nil)
	})

	// Zero is off, which is the previous behaviour: every lifetime is listed,
	// and the oldest is what a player would start on.
	t.Run("zero lists every lifetime", func(t *testing.T) {
		got := render(Options{})
		listed(t, got, append(ended, live...), nil)
		assert.Contains(t, got, "#EXT-X-MEDIA-SEQUENCE:0")
	})
}

// newEpochFixture builds a track of three producer lifetimes, returning the
// groups of each in order.
func newEpochFixture(tb testing.TB, perEpoch ...int64) (*ledger.Track, [][]ledger.GroupInfo) {
	tb.Helper()
	ctx := context.Background()

	store := mem.New()
	track, err := ledger.Create(ctx, store, "live/cam1/video", ledger.TrackSchema{
		Timescale: 90000, TimeSource: ledger.TimeSourceFrame,
		MIME: "video/mp4", Encoding: "fmp4",
	}, ledger.Config{})
	require.NoError(tb, err)

	writer, err := track.Writer(ctx)
	require.NoError(tb, err)

	var epochs [][]ledger.GroupInfo
	for lifetime, n := range perEpoch {
		if lifetime > 0 {
			require.NoError(tb, writer.NewEpoch(ctx))
		}
		var groups []ledger.GroupInfo
		for i := range n {
			meta, err := writer.AppendGroup(ctx, ledger.GroupInfo{
				ID:        ledger.NewGroupID(0, uint64(i)),
				MediaTime: i * 180000,
				Duration:  180000,
				Wallclock: fixtureEpoch.Add(time.Duration(i) * 2 * time.Second).UnixNano(),
			}, []byte("frames"))
			require.NoError(tb, err)
			groups = append(groups, meta)
		}
		epochs = append(epochs, groups)
	}
	return track, epochs
}

// renderManifest serves one manifest through a handler built with opts.
func renderManifest(tb testing.TB, track *ledger.Track, name string, opts Options) string {
	tb.Helper()

	if opts.InitSegment.Bytes == nil && opts.InitSegment.URL == "" {
		opts.InitSegment = testInit
	}
	handler, err := NewHandler(track, opts)
	require.NoError(tb, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/" + name)
	require.NoError(tb, err)
	defer resp.Body.Close()
	require.Equal(tb, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(tb, err)
	return string(body)
}

// The two windows bound different axes and have to compose. A segment window can
// already have trimmed a lifetime away, and the epoch window counts what the
// manifest still lists rather than epoch numbers — so it must not evict a
// session that is on screen, nor keep one that is not.
func TestHandler_ServeHTTP_WindowAndEpochWindow(t *testing.T) {
	// Three lifetimes of 3, 4 and 2 segments. A window of 3 reaches back exactly
	// one segment into the middle lifetime.
	track, epochs := newEpochFixture(t, 3, 4, 2)
	middle, live := epochs[1], epochs[2]

	t.Run("two lifetimes keeps the segment reaching back", func(t *testing.T) {
		got := renderManifest(t, track, "playlist.m3u8", Options{Window: 3, EpochWindow: 2})

		assert.Contains(t, got, middle[len(middle)-1].ID.String()+".m4s",
			"the middle lifetime is still listed, so its segment in the window stays")
		assert.Contains(t, got, "#EXT-X-MEDIA-SEQUENCE:6")
	})

	t.Run("one lifetime drops it", func(t *testing.T) {
		got := renderManifest(t, track, "playlist.m3u8", Options{Window: 3, EpochWindow: 1})

		assert.NotContains(t, got, middle[len(middle)-1].ID.String()+".m4s")
		for _, meta := range live {
			assert.Contains(t, got, meta.ID.String()+".m4s")
		}
		assert.Contains(t, got, "#EXT-X-MEDIA-SEQUENCE:7",
			"the segment from the ended lifetime is behind the manifest too")
	})

	// The epoch window counts listed lifetimes, so asking for more than the
	// window shows cannot reach back past it.
	t.Run("more lifetimes than the window shows", func(t *testing.T) {
		got := renderManifest(t, track, "playlist.m3u8", Options{Window: 3, EpochWindow: 3})

		assert.Contains(t, got, "#EXT-X-MEDIA-SEQUENCE:6",
			"the segment window still bounds the manifest")
		assert.NotContains(t, got, epochs[0][0].ID.String()+".m4s",
			"a lifetime the segment window already trimmed is not pulled back in")
	})
}

// Epoch scoping is a property of the manifest, not of HLS: the MPD lists the
// same segments and must drop an ended lifetime the same way.
func TestHandler_ServeHTTP_EpochWindowDASH(t *testing.T) {
	track, epochs := newEpochFixture(t, 3, 2)
	ended, live := epochs[0], epochs[1]

	got := renderManifest(t, track, "manifest.mpd", Options{EpochWindow: 1})

	for _, meta := range ended {
		assert.NotContains(t, got, `media="`+meta.ID.String()+`.m4s"`,
			"a segment from the finished lifetime is not listed")
	}
	for _, meta := range live {
		assert.Contains(t, got, `media="`+meta.ID.String()+`.m4s"`)
	}
	assert.Contains(t, got, `<S t="0"`,
		"the first listed segment anchors the timeline, since media time resets per epoch")
}

// The smallest window is one segment, where the ring wraps on every group.
func TestHandler_ServeHTTP_WindowOfOne(t *testing.T) {
	fix := newTrackFixtureGroups(t, "fmp4", 5)

	got := renderManifest(t, fix.track, "playlist.m3u8", Options{Window: 1})

	assert.Contains(t, got, fix.metas[4].ID.String()+".m4s", "only the newest segment")
	for _, meta := range fix.metas[:4] {
		assert.NotContains(t, got, meta.ID.String()+".m4s")
	}
	assert.Contains(t, got, "#EXT-X-MEDIA-SEQUENCE:4")
}

// An init segment given as a URL is referenced by both manifests and is not
// served by the handler: it already lives somewhere the client can reach.
func TestHandler_ServeHTTP_InitSegmentURL(t *testing.T) {
	fix := newTrackFixture(t)
	const url = "https://objects.example/live/cam1/init.m4s"
	opts := Options{InitSegment: InitSegment{URL: url}}

	assert.Contains(t, renderManifest(t, fix.track, "playlist.m3u8", opts),
		`#EXT-X-MAP:URI="`+url+`"`)
	assert.Contains(t, renderManifest(t, fix.track, "manifest.mpd", opts),
		`<Initialization sourceURL="`+url+`"/>`)

	handler, err := NewHandler(fix.track, opts)
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/init.m4s")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"the handler holds no bytes to serve for a URL-referenced init")
}

// playlistFor renders the HLS playlist for a track through a windowed handler.
func playlistFor(tb testing.TB, track *ledger.Track, window int) string {
	tb.Helper()

	handler, err := NewHandler(track, Options{
		InitSegment: testInit,
		Window:      window,
	})
	require.NoError(tb, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/playlist.m3u8")
	require.NoError(tb, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(tb, err)
	return string(body)
}

// A windowed MPD lists only the window, and says how far back it reaches. Its
// availabilityStartTime anchors the presentation, so it must name where the
// presentation began rather than where the window currently starts — otherwise
// it moves on every refresh and drags each client's timeline with it.
func TestHandler_ServeHTTP_WindowDASH(t *testing.T) {
	const total = 10
	fix := newTrackFixtureGroups(t, "fmp4", total)

	manifestFor := func(window int) string {
		handler, err := NewHandler(fix.track, Options{
			InitSegment: testInit,
			Window:      window,
		})
		require.NoError(t, err)
		ts := httptest.NewServer(handler)
		defer ts.Close()

		resp, err := http.Get(ts.URL + "/manifest.mpd")
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return string(body)
	}

	got := manifestFor(3)

	assert.Contains(t, got, `timeShiftBufferDepth="PT6S"`,
		"three two-second segments is how far back the window reaches")
	for _, meta := range fix.metas[:total-3] {
		assert.NotContains(t, got, `media="`+meta.ID.String()+`.m4s"`,
			"segments older than the window are not listed")
	}
	for _, meta := range fix.metas[total-3:] {
		assert.Contains(t, got, `media="`+meta.ID.String()+`.m4s"`)
	}

	// The presentation did not move, so neither may its anchor.
	assert.Equal(t,
		availabilityStartTime(t, manifestFor(0)),
		availabilityStartTime(t, got),
		"availabilityStartTime names the presentation start, not the window start")
}

// availabilityStartTime pulls the MPD attribute out of a rendered manifest.
func availabilityStartTime(tb testing.TB, mpd string) string {
	tb.Helper()

	const attr = `availabilityStartTime="`
	at := strings.Index(mpd, attr)
	require.GreaterOrEqual(tb, at, 0, "the MPD carries an availabilityStartTime")
	rest := mpd[at+len(attr):]
	end := strings.Index(rest, `"`)
	require.GreaterOrEqual(tb, end, 0)
	return rest[:end]
}

// A supplied init segment is served at /init.<ext> and referenced from both
// manifests.
func TestHandler_ServeHTTP_InitSegment(t *testing.T) {
	fix := newTrackFixture(t)
	handler, err := NewHandler(fix.track, Options{
		InitSegment: InitSegment{Bytes: []byte("init-bytes")},
	})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/init.m4s")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, []byte("init-bytes"), body)

	// Both manifests reference it.
	for _, name := range []string{"playlist.m3u8", "manifest.mpd"} {
		resp, err := http.Get(ts.URL + "/" + name)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)
		assert.Contains(t, string(body), "init.m4s", "%s must reference the init segment", name)
	}
}

// A store outage is a failure to answer, not an absence: serving 404 for a
// segment that exists would tell a player to prune it. The body is exactly the
// status text — the store errors underneath carry object keys and store paths,
// which have no business in a response.
func TestHandler_ServeHTTP_StoreFailureIsServerError(t *testing.T) {
	backend := &fakeStore{}
	fix := newTrackFixtureStore(t, "fmp4", 2, backend)
	backend.failAll = true

	handler, err := NewHandler(fix.track, Options{InitSegment: testInit})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	for name, path := range map[string]string{
		"segment":  "/" + fix.metas[0].ID.String() + ".m4s",
		"playlist": "/playlist.m3u8",
		"manifest": "/manifest.mpd",
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := http.Get(ts.URL + path)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, http.StatusInternalServerError, resp.StatusCode,
				"a store outage must not read as a missing segment")

			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			// http.Error terminates with a newline; the point is that the body
			// is exactly this and nothing of the store error.
			assert.Equal(t, http.StatusText(http.StatusInternalServerError)+"\n", string(body))
		})
	}
}

// The Lookup split itself, which the outage-all fixture cannot reach: Reader
// construction loads only epoch 1's log, so failing epoch 2's log key lets the
// reader open cleanly and makes Lookup fail for a reason that is neither
// ErrGroupNotFound nor the open — the arm that must answer 500 rather than 404.
func TestHandler_ServeHTTP_LookupFailureIsServerError(t *testing.T) {
	backend := &fakeStore{}
	fix := newTrackFixtureStore(t, "fmp4", 1, backend)

	// A second lifetime, so the fixture's groups sit in epoch 1 and a segment
	// in epoch 2 forces Lookup to fetch a log the open never touched.
	ctx := context.Background()
	writer, err := fix.track.Writer(ctx)
	require.NoError(t, err)
	require.NoError(t, writer.NewEpoch(ctx))
	restarted, err := writer.AppendGroup(ctx, ledger.GroupInfo{
		ID:        ledger.NewGroupID(0, 1),
		MediaTime: 0,
		Duration:  180000,
	}, []byte("frames"))
	require.NoError(t, err)

	// Armed only now: NewEpoch itself reads epoch 2's log back after writing it.
	backend.failKey = "live/cam1/video/e000002/log.manifest"

	handler, err := NewHandler(fix.track, Options{InitSegment: testInit})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/" + restarted.ID.String() + ".m4s")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode,
		"a Lookup failure that is not an absence answers 500")
}

// assertGenericServerError asserts resp is a 500 whose body is exactly the
// status text. The errors behind these responses carry object keys and store
// paths, so the body is checked as closely as the status.
func assertGenericServerError(tb testing.TB, resp *http.Response) {
	tb.Helper()
	assert.Equal(tb, http.StatusInternalServerError, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(tb, err)
	// http.Error terminates with a newline.
	assert.Equal(tb, http.StatusText(http.StatusInternalServerError)+"\n", string(body))
}

// The read after a successful Lookup: the manifest row is there but the payload
// object cannot be fetched. The segment exists, so this is a 500, not a 404.
func TestHandler_ServeHTTP_ReadGroupFailureIsServerError(t *testing.T) {
	backend := &fakeStore{}
	fix := newTrackFixtureStore(t, "fmp4", 2, backend)
	backend.failKey = fix.metas[0].ObjectKey

	handler, err := NewHandler(fix.track, Options{InitSegment: testInit})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/" + fix.metas[0].ID.String() + ".m4s")
	require.NoError(t, err)
	defer resp.Body.Close()
	assertGenericServerError(t, resp)

	// The other segment's payload is untouched and still serves.
	resp, err = http.Get(ts.URL + "/" + fix.metas[1].ID.String() + ".m4s")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// The manifest walk failing partway: the reader opens on epoch 1's log, and the
// walk into epoch 2 hits a log that cannot be fetched. A truncated manifest
// would tell a player the stream ended there, so neither format is served.
func TestHandler_ServeHTTP_GatherFailureIsServerError(t *testing.T) {
	backend := &fakeStore{}
	fix := newTrackFixtureStore(t, "fmp4", 1, backend)

	ctx := context.Background()
	writer, err := fix.track.Writer(ctx)
	require.NoError(t, err)
	require.NoError(t, writer.NewEpoch(ctx))
	_, err = writer.AppendGroup(ctx, ledger.GroupInfo{
		ID:        ledger.NewGroupID(0, 1),
		MediaTime: 0,
		Duration:  180000,
	}, []byte("frames"))
	require.NoError(t, err)

	// Armed only now: NewEpoch itself reads epoch 2's log back after writing it.
	backend.failKey = "live/cam1/video/e000002/log.manifest"

	handler, err := NewHandler(fix.track, Options{InitSegment: testInit})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// The reader still opens: an epoch 1 segment serves, so the failures below
	// come from the walk reaching epoch 2 and not from opening the track.
	resp, err := http.Get(ts.URL + "/" + fix.metas[0].ID.String() + ".m4s")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	for name, manifest := range map[string]string{
		"playlist": "/playlist.m3u8",
		"manifest": "/manifest.mpd",
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := http.Get(ts.URL + manifest)
			require.NoError(t, err)
			defer resp.Body.Close()
			assertGenericServerError(t, resp)
		})
	}
}

// A resolver that cannot produce a URL — a signing backend that is down, say —
// is a failure to answer. The handler must not fall back to proxying the bytes
// a deployment chose not to serve itself, nor report the segment missing.
func TestHandler_ServeHTTP_ResolverFailureIsServerError(t *testing.T) {
	fix := newTrackFixture(t)
	handler, err := NewHandler(fix.track, Options{
		InitSegment: testInit,
		Resolver:    &fakeResolver{err: errors.New("signer unavailable: key live/cam1/video")},
	})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/" + fix.metas[0].ID.String() + ".m4s")
	require.NoError(t, err)
	defer resp.Body.Close()
	assertGenericServerError(t, resp)
}

// A name that is not a group id never reaches the ledger: it is answered 404
// from the URL alone. The store is failing throughout, so a request that did
// reach it would answer 500 instead.
func TestHandler_ServeHTTP_MalformedSegmentNotFound(t *testing.T) {
	backend := &fakeStore{}
	fix := newTrackFixtureStore(t, "fmp4", 2, backend)
	backend.failAll = true

	handler, err := NewHandler(fix.track, Options{InitSegment: testInit})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	for name, segment := range map[string]string{
		"not a group id":  "/segment.m4s",
		"zero id":         "/e000000-g00000000.m4s",
		"wrong extension": "/" + fix.metas[0].ID.String() + ".ts",
		"no name":         "/",
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := http.Get(ts.URL + segment)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		})
	}
}

// A group without a duration cannot be given an EXTINF or an @d. Rendering it
// anyway would publish a manifest a player cannot time, so both formats refuse.
func TestHandler_ServeHTTP_GroupWithoutDurationIsServerError(t *testing.T) {
	fix := newTrackFixture(t)

	var buf bytes.Buffer
	handler, err := NewHandler(fix.track, Options{
		InitSegment: testInit,
		Logger:      slog.New(slog.NewTextHandler(&buf, nil)),
	})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	manifests := map[string]string{
		"playlist": "/playlist.m3u8",
		"manifest": "/manifest.mpd",
	}

	// Every group has a duration so far, and both manifests render.
	for name, manifest := range manifests {
		resp, err := http.Get(ts.URL + manifest)
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s before the append", name)
	}

	ctx := context.Background()
	writer, err := fix.track.Writer(ctx)
	require.NoError(t, err)
	_, err = writer.AppendGroup(ctx, ledger.GroupInfo{
		ID:        ledger.NewGroupID(0, 2),
		MediaTime: 2 * 180000,
	}, []byte("frames"))
	require.NoError(t, err)

	for name, manifest := range manifests {
		t.Run(name, func(t *testing.T) {
			buf.Reset()
			resp, err := http.Get(ts.URL + manifest)
			require.NoError(t, err)
			defer resp.Body.Close()
			assertGenericServerError(t, resp)
			assert.Contains(t, buf.String(), "has no duration",
				"the 500 is the renderer refusing the group, not some other failure")
		})
	}
}

// With a logger supplied, the generic 500 is accompanied by a record carrying
// what broke and which request broke it — the log is the only place the detail
// survives.
func TestHandler_ServeHTTP_InternalErrorsAreLogged(t *testing.T) {
	backend := &fakeStore{}
	fix := newTrackFixtureStore(t, "fmp4", 2, backend)
	backend.failAll = true

	var buf bytes.Buffer
	handler, err := NewHandler(fix.track, Options{
		InitSegment: testInit,
		Logger:      slog.New(slog.NewTextHandler(&buf, nil)),
	})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/" + fix.metas[0].ID.String() + ".m4s")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	logged := buf.String()
	assert.Contains(t, logged, `error="`, "the error is logged under the attr name the ledger's records use")
	assert.Contains(t, logged, "store outage", "the underlying error is in the record, wrapping and all")
	assert.Contains(t, logged, "GET", "the record names the method")
	assert.Contains(t, logged, "/"+fix.metas[0].ID.String()+".m4s", "the record names the request path")
}

// A canceled request is player churn — a seek, a variant switch — not a
// failure: the handler still answers 500, but writes no record, so the log the
// option carries stays a signal of real outages.
func TestHandler_ServeHTTP_CanceledRequestNotLogged(t *testing.T) {
	backend := &fakeStore{}
	fix := newTrackFixtureStore(t, "fmp4", 2, backend)
	backend.failAll = true
	backend.err = context.Canceled

	var buf bytes.Buffer
	handler, err := NewHandler(fix.track, Options{
		InitSegment: testInit,
		Logger:      slog.New(slog.NewTextHandler(&buf, nil)),
	})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/" + fix.metas[0].ID.String() + ".m4s")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Empty(t, buf.String(), "routine client churn writes no record")
}

func TestNewHandler_QueryParam(t *testing.T) {
	tests := map[string]struct {
		param   string
		wantErr bool
	}{
		"none":                 {},
		"a variable name":      {param: "cdn-token_2"},
		"a character it bars":  {param: "jwt=", wantErr: true},
		"a space":              {param: "my token", wantErr: true},
		"a substitution brace": {param: "{$jwt}", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fix := newTrackFixtureEncoding(t, "ts")

			handler, err := NewHandler(fix.track, Options{QueryParam: tt.param})

			if tt.wantErr {
				assert.Error(t, err)
				assert.Nil(t, handler)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, handler)
		})
	}
}

// The handler's playlist carries its query parameter onto the init segment
// it serves and onto every segment.
func TestHandler_ServeHTTP_QueryParam(t *testing.T) {
	fix := newTrackFixture(t)
	handler, err := NewHandler(fix.track, Options{InitSegment: testInit, QueryParam: "jwt"})
	require.NoError(t, err)
	segment := fix.metas[0].ID.String() + ".m4s"

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/track/playlist.m3u8?jwt=t", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `#EXT-X-DEFINE:QUERYPARAM="jwt"`)
	assert.Contains(t, rec.Body.String(), `#EXT-X-MAP:URI="init.m4s?jwt={$jwt}"`)
	assert.Contains(t, rec.Body.String(), segment+"?jwt={$jwt}")

	served := httptest.NewRecorder()
	handler.ServeHTTP(served, httptest.NewRequest(http.MethodGet, "/track/"+segment+"?jwt=t", nil))
	assert.Equal(t, http.StatusOK, served.Code, "a segment URL with the query still resolves")
	assert.Equal(t, fix.payload[0], served.Body.Bytes())
}
