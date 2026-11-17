package abr

import (
	"context"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/gomoqt/msf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/cmaf"
	"github.com/qumo-dev/qumo/internal/optin"
)

// serveOneGroup serves body as the one frame of one group, then holds the
// track open until the subscriber leaves.
func serveOneGroup(tw *moqt.TrackWriter, body []byte) {
	gw, err := tw.OpenGroup(tw.Context())
	if err != nil {
		return
	}
	frame := moqt.NewFrame(len(body))
	_, _ = frame.Write(body) // not actionable: the test asserts what the pipeline read
	_ = gw.WriteFrame(frame) // not actionable: as above
	_ = gw.Close()           // not actionable: as above
	<-tw.Context().Done()
}

// A browser's source (Annex B H.264, no size in its catalog) is published
// through the whole pipeline with no size or label until its first keyframe,
// then with the size decoded from that keyframe's parameter sets. Its ladder
// has no rung below the source, so no encoder (nor ffmpeg) starts.
func TestRun_AnnexBSourceSizedAtFirstKeyframe(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	catalog, err := msf.Catalog{Version: 1, Tracks: []msf.Track{{
		Name: "video", Role: msf.RoleVideo, Packaging: msf.PackagingLOC, IsLive: new(true), Codec: "avc1.42c01e",
	}}}.MarshalJSON()
	require.NoError(t, err)
	sps, pps := parameterSets(t, avcRecord360p)
	keyframe := cmaf.EncodeLOC(1_000_000, annexB(sps, pps, []byte{0x65, 0x88, 0x84, 0x00}))
	release := make(chan struct{})

	source := moqt.NewTrackMux(0)
	source.PublishFunc(ctx, "/live/cam", func(tw *moqt.TrackWriter) {
		switch tw.TrackName {
		case msf.DefaultCatalogTrackName:
			serveOneGroup(tw, catalog)
		case "video":
			select {
			case <-release:
				serveOneGroup(tw, keyframe)
			case <-tw.Context().Done():
			}
		default:
			tw.CloseWithError(moqt.SubscribeErrorCodeNotFound)
		}
	})
	source.Publish(ctx, "/.abr/live/cam", optin.Handler("/live/cam"))
	sess := loopbackSession(t, ctx, source)

	out := moqt.NewTrackMux(0)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, sess, out, "/.abr/live/cam", "/live/cam", "/live/cam.abr", Config{Ladder: []Rung{{Height: 720, Bitrate: 2_500_000}}})
	}()
	sourceRendition := func() (msf.Track, bool) {
		_, h := out.TrackHandler("/live/cam.abr")
		o, ok := h.(*outputBroadcast)
		if !ok {
			return msf.Track{}, false
		}
		for _, tr := range o.b.Catalog().Tracks {
			if tr.Name == "video" {
				return tr, true
			}
		}
		return msf.Track{}, false
	}

	require.Eventually(t, func() bool { _, ok := sourceRendition(); return ok }, 10*time.Second, 20*time.Millisecond,
		"the source rendition is published before its first keyframe")
	before, _ := sourceRendition()
	assert.Nil(t, before.Width, "no size is known yet")
	assert.Nil(t, before.Height)
	assert.Empty(t, before.Label)

	close(release)
	require.Eventually(t, func() bool { tr, _ := sourceRendition(); return tr.Height != nil }, 10*time.Second, 20*time.Millisecond,
		"sized at the first keyframe")
	after, _ := sourceRendition()
	require.NotNil(t, after.Width)
	assert.EqualValues(t, 640, *after.Width)
	assert.EqualValues(t, 360, *after.Height)
	assert.Equal(t, "360p (source)", after.Label)
	require.NotNil(t, after.AltGroup)
	assert.EqualValues(t, videoAltGroup, *after.AltGroup)

	cancel()
	<-done
}
