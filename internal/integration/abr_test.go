//go:build integration

package integration

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/gomoqt/msf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/abr"
	"github.com/qumo-dev/qumo/internal/cmaf"
	"github.com/qumo-dev/qumo/internal/ffpub"
	"github.com/qumo-dev/qumo/internal/hlsoptin"
	"github.com/qumo-dev/qumo/internal/ingest"
)

// TestABR_RenditionsBesideSource publishes a 720p broadcast over RTMP to an
// ingest behind a relay, runs the ABR transcoder against the relay, and
// subscribes to the ABR broadcast it publishes: the catalog lists the source
// video and each rendition shorter than it in one alternate group, and every
// video track numbers its groups as the source does, each group starting at
// the same frame with a keyframe.
func TestABR_RenditionsBesideSource(t *testing.T) {
	if !ffpub.Available() {
		t.Skip("ffmpeg not on PATH")
	}
	const (
		src   = moqt.BroadcastPath("/live/abrtest")
		dst   = moqt.BroadcastPath("/live/abrtest.abr")
		alias = moqt.BroadcastPath("/.hls/live/abrtest")
	)
	addr, srv := startAuthRelay(t, authOff, nil)
	url := "https://" + addr + "/"

	// The source: ffmpeg over RTMP into an ingest that publishes through the
	// relay.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ingestMux := moqt.NewTrackMux(0)
	rtmpLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	rtmpAddr := rtmpLn.Addr().String()
	require.NoError(t, rtmpLn.Close())
	go func() {
		_ = ingest.NewRTMPServer(ingest.RTMPConfig{Addr: rtmpAddr, TrackMux: ingestMux}).ListenAndServe(ctx)
	}()
	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp", rtmpAddr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
		}
		return err == nil
	}, 5*time.Second, 50*time.Millisecond)
	dialOver(t, url, nil, ingestMux)

	pub := ffpub.New(ffpub.Config{
		URL:   fmt.Sprintf("rtmp://%s%s", rtmpAddr, src),
		Audio: true, GOP: 30, Width: 1280, Height: 720, Framerate: 30,
	})
	pubCtx, stopPub := context.WithCancel(ctx)
	defer stopPub()
	require.NoError(t, pub.Start(pubCtx))
	// An HLS opt-in for the source: an alias, which redirects every
	// subscription to it, and which is not to be transcoded again.
	ingestMux.Publish(ctx, alias, hlsoptin.Handler(src))
	t.Cleanup(func() {
		stopPub() // ffmpeg publishes until its context ends
		_ = pub.Wait()
	})
	require.Eventually(t, routed(srv, src), 10*time.Second, 50*time.Millisecond)

	// The transcoder.
	tc := &abr.Transcoder{
		Prefix: "/",
		Suffix: ".abr",
		Config: abr.Config{
			Ladder:  []abr.Rung{{Height: 360, Bitrate: 800_000}, {Height: 240, Bitrate: 400_000}},
			Encoder: abr.EncoderConfig{FFmpeg: "ffmpeg", Codec: "libx264", Preset: "ultrafast"},
		},
	}
	abrMux := moqt.NewTrackMux(0)
	abrSess := dialOver(t, url, nil, abrMux)
	go func() { _ = tc.Serve(ctx, abrSess, abrMux) }()
	require.Eventually(t, routed(srv, dst), 10*time.Second, 50*time.Millisecond)
	require.Never(t, routed(srv, alias+".abr"), 3*time.Second, 100*time.Millisecond, "the alias was transcoded")

	// The viewer.
	viewer := dialOver(t, url, nil, moqt.NewTrackMux(0))
	videoTracks := []string{"video", "video-360p", "video-240p"}
	catalog := waitCatalog(t, viewer, dst, append(videoTracks, "audio"))

	byName := map[string]msf.Track{}
	for _, tr := range catalog.Tracks {
		byName[tr.Name] = tr
	}
	for _, name := range videoTracks {
		tr := byName[name]
		require.NotNil(t, tr.AltGroup, name)
		assert.EqualValues(t, 1, *tr.AltGroup, name)
		assert.NotEmpty(t, tr.InitRef, name)
		assert.Contains(t, tr.Codec, "avc1.", name)
		require.NotNil(t, tr.Height, name)
	}
	assert.EqualValues(t, 720, *byName["video"].Height)
	assert.EqualValues(t, 360, *byName["video-360p"].Height)
	assert.EqualValues(t, 640, *byName["video-360p"].Width)
	assert.EqualValues(t, 240, *byName["video-240p"].Height)
	assert.Nil(t, byName["audio"].AltGroup)

	// Read each video track's groups for a while.
	groups := make([]map[moqt.GroupSequence]obsGroup, len(videoTracks))
	var wg sync.WaitGroup
	readCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	for i, name := range videoTracks {
		wg.Go(func() { groups[i] = readGroups(readCtx, t, viewer, dst, moqt.TrackName(name)) })
	}
	wg.Wait()

	// Every group the source and a rendition both delivered whole starts at
	// the same timestamp, with a keyframe, and holds the same frames.
	for i, name := range videoTracks[1:] {
		common := 0
		for seq, want := range groups[0] {
			got, ok := groups[i+1][seq]
			if !ok || !want.complete || !got.complete {
				continue
			}
			common++
			require.NotEmpty(t, got.frames, "%s group %d", name, seq)
			assert.True(t, got.frames[0].keyframe, "%s group %d does not open with a keyframe", name, seq)
			require.Equal(t, len(want.frames), len(got.frames), "%s group %d frame count", name, seq)
			for k := range want.frames {
				assert.Equal(t, want.frames[k].timestamp, got.frames[k].timestamp, "%s group %d frame %d", name, seq, k)
			}
		}
		assert.GreaterOrEqual(t, common, 2, "%s shares too few whole groups with the source", name)
	}

	t.Run("audio is forwarded", func(t *testing.T) {
		audioCtx, stop := context.WithTimeout(ctx, 2*time.Second)
		defer stop()
		audio := readGroups(audioCtx, t, viewer, dst, "audio")
		frames := 0
		for _, g := range audio {
			frames += len(g.frames)
		}
		assert.Greater(t, frames, 20, "2 s of AAC is ~90 frames")
	})

	t.Run("the source's measured bitrate is stated", func(t *testing.T) {
		c := waitCatalogWhere(t, viewer, dst, "video with a bitrate", func(c msf.Catalog) bool {
			for _, tr := range c.Tracks {
				if tr.Name == "video" && tr.Bitrate != nil {
					return true
				}
			}
			return false
		})
		for _, tr := range c.Tracks {
			if tr.Name == "video" {
				// ffpub encodes 720p30 with libx264 veryfast at its default
				// rate: well above 360p's 800 kbit/s and below 50 Mbit/s.
				assert.Greater(t, *tr.Bitrate, int64(800_000))
				assert.Less(t, *tr.Bitrate, int64(50_000_000))
			}
		}
	})

	t.Run("the ABR broadcast ends with its source", func(t *testing.T) {
		require.True(t, routed(srv, dst)(), "routed while the source plays")
		stopPub()
		require.Eventually(t, func() bool { return !routed(srv, dst)() }, 15*time.Second, 100*time.Millisecond,
			"the ABR broadcast outlived its source")
	})
}

// waitCatalog subscribes to path's catalog and returns the first one that
// lists every track in want.
func waitCatalog(t *testing.T, sess *moqt.Session, path moqt.BroadcastPath, want []string) msf.Catalog {
	t.Helper()
	return waitCatalogWhere(t, sess, path, fmt.Sprint("listing ", want), func(c msf.Catalog) bool {
		return listsTracks(c, want)
	})
}

// waitCatalogWhere subscribes to path's catalog and returns the first one ok
// accepts, the catalog being sent again on every change.
func waitCatalogWhere(t *testing.T, sess *moqt.Session, path moqt.BroadcastPath, what string, ok func(msf.Catalog) bool) msf.Catalog {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tr, err := sess.Subscribe(ctx, path, msf.DefaultCatalogTrackName, nil)
	require.NoError(t, err)
	defer tr.Close()
	f := moqt.NewFrame(0)
	for {
		gr, err := tr.AcceptGroup(ctx)
		require.NoError(t, err, "no catalog %s", what)
		if err := gr.ReadFrame(f); err != nil {
			continue
		}
		c, err := msf.ParseCatalog(f.Body())
		require.NoError(t, err)
		if ok(c) {
			return c
		}
	}
}

// obsFrame is a video frame as a viewer received it.
type obsFrame struct {
	timestamp uint64
	keyframe  bool
}

// obsGroup is a group as a viewer received it.
type obsGroup struct {
	frames   []obsFrame
	complete bool // read to its end
}

// readGroups reads track name's groups until ctx ends.
func readGroups(ctx context.Context, t *testing.T, sess *moqt.Session, path moqt.BroadcastPath, name moqt.TrackName) map[moqt.GroupSequence]obsGroup {
	tr, err := sess.Subscribe(ctx, path, name, &moqt.SubscribeConfig{Ordered: true})
	if err != nil {
		t.Errorf("subscribe %s: %v", name, err)
		return nil
	}
	defer tr.Close()
	out := map[moqt.GroupSequence]obsGroup{}
	f := moqt.NewFrame(0)
	for {
		gr, err := tr.AcceptGroup(ctx)
		if err != nil {
			return out
		}
		var g obsGroup
		for {
			if err := gr.ReadFrame(f); err != nil {
				g.complete = errors.Is(err, io.EOF)
				break
			}
			// readGroups runs on its own goroutine, where require's
			// FailNow must not be called.
			ts, data, err := cmaf.DecodeLOC(f.Body())
			if err != nil {
				t.Errorf("%s group %d: %v", name, gr.GroupSequence(), err)
				return out
			}
			g.frames = append(g.frames, obsFrame{timestamp: ts, keyframe: hasIDR(data)})
		}
		out[gr.GroupSequence()] = g
	}
}

// hasIDR reports whether AVCC data (4-byte lengths) holds an IDR slice.
func hasIDR(data []byte) bool {
	for len(data) >= 4 {
		n := int(binary.BigEndian.Uint32(data))
		data = data[4:]
		if n <= 0 || n > len(data) {
			return false
		}
		if data[0]&0x1f == 5 {
			return true
		}
		data = data[n:]
	}
	return false
}
