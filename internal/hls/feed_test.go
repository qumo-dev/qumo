package hls

import (
	"context"
	"encoding/base64"
	"testing"
	"testing/synctest"
	"time"

	"github.com/qumo-dev/gomoqt/msf"

	"github.com/okdaichi/qumo-ledger/ledger"
	"github.com/qumo-dev/qumo/internal/cmaf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// groupInfo places a group at the media time the caller has accumulated, so a
// dropped MoQ group (a gappy sequence) leaves no hole in the timeline. The
// sequence is the group's identity; the epoch is stamped by the writer.
func Test_groupInfo(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	const du int64 = 180000 // two seconds at 90 kHz

	tests := map[string]struct {
		seq       int64
		mediaTime int64
	}{
		"first group":  {seq: 5, mediaTime: 0},
		"second group": {seq: 6, mediaTime: 180000},
		"gappy seq":    {seq: 99, mediaTime: 360000},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := groupInfo(uint64(tt.seq), tt.mediaTime, du, 30, now)

			assert.Equal(t, ledger.NewGroupID(0, uint64(tt.seq)), got.ID,
				"the producer sequence is the identity; the epoch is stamped by the writer")
			assert.Equal(t, tt.mediaTime, got.MediaTime,
				"media time is the caller's running total, not the gappy producer sequence")
			assert.Equal(t, du, got.Duration)
			assert.Equal(t, now.UnixNano(), got.Wallclock)
			assert.Equal(t, uint64(30), got.ObjectCount)
		})
	}
}

// trackSchema describes what the egress stores — the fragments it packages —
// rather than what arrived over the wire.
func Test_trackSchema(t *testing.T) {
	s := trackSchema(&msf.Track{
		Name: "video", Packaging: msf.PackagingLOC,
		MimeType: "video/mp4", Codec: "vp09.00.10.08",
	})

	assert.Equal(t, uint32(cmaf.Timescale), s.Timescale,
		"the packager's timescale, since the packager writes the payloads")
	assert.Equal(t, "fmp4", s.Encoding)
	assert.Equal(t, "video/mp4", s.MIME)
	assert.Equal(t, ledger.TimeSourceIngest, s.TimeSource)
}

// packagerForTrack needs a picture size to describe the track; a catalog that
// omits one is a publisher that is not ready.
func Test_packagerForTrack(t *testing.T) {
	w, h := int64(1280), int64(720)

	p, err := packagerForTrack(msf.Catalog{}, &msf.Track{
		Name: "video", Packaging: msf.PackagingLOC,
		Codec: "vp09.00.10.08", Width: &w, Height: &h,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, p.Init(), "the init segment comes from the catalog, not from media")

	_, err = packagerForTrack(msf.Catalog{}, &msf.Track{Name: "video", Codec: "vp09.00.10.08"})
	assert.Error(t, err, "no picture size")
}

// initFromTrack base64-decodes the catalog's InitDataList entry the track's
// initRef points at (the fMP4 init), tolerating its absence or malformed values.
func Test_initFromTrack(t *testing.T) {
	want := []byte("fmp4-init-bytes")
	catalog := msf.Catalog{InitDataList: []msf.InitDataRef{
		{ID: "video", Type: "inline", Data: base64.StdEncoding.EncodeToString(want)},
		{ID: "bad", Type: "inline", Data: "!!!not-base64!!!"},
	}}

	assert.Equal(t, want, initFromTrack(catalog, &msf.Track{InitRef: "video"}))
	assert.Nil(t, initFromTrack(catalog, &msf.Track{}), "no InitRef yields no init")
	assert.Nil(t, initFromTrack(catalog, &msf.Track{InitRef: "missing"}), "unresolved InitRef yields no init")
	assert.Nil(t, initFromTrack(catalog, &msf.Track{InitRef: "bad"}), "malformed init data yields no init")
}

// wallclockAt derives a group's wall-clock anchor from its media time, so the
// timeline a manifest publishes advances with the media rather than with
// whatever gap the network added between arrivals.
func Test_wallclockAt(t *testing.T) {
	anchor := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	const timescale = uint32(1_000_000) // microseconds

	tests := map[string]struct {
		mediaTime int64
		want      time.Time
	}{
		"at the anchor":    {mediaTime: 0, want: anchor},
		"one second in":    {mediaTime: 1_000_000, want: anchor.Add(time.Second)},
		"a fractional gap": {mediaTime: 1_006_500, want: anchor.Add(1_006_500 * time.Microsecond)},
		"an hour of media": {mediaTime: 3600 * 1_000_000, want: anchor.Add(time.Hour)},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, wallclockAt(anchor, tt.mediaTime, timescale))
		})
	}
}

// findTrack selects a track by name from the catalog.
func Test_findTrack(t *testing.T) {
	c := msf.Catalog{Tracks: []msf.Track{{Name: "video"}, {Name: "audio"}}}

	require.NotNil(t, findTrack(c, "video"))
	assert.Equal(t, "video", findTrack(c, "video").Name)
	assert.Nil(t, findTrack(c, "missing"))
}

// testMediaInfo is a media track the real VP9 packager can describe, so the
// orchestration tests exercise packaging rather than a stub. VP9 carries its
// config in the codec string, so no fMP4 init (Description) is needed.
func testMediaInfo(tb testing.TB) mediaInfo {
	tb.Helper()
	p, err := cmaf.NewPackager(cmaf.VideoConfig{
		Codec: "vp09.00.10.08", Width: 1280, Height: 720,
	})
	require.NoError(tb, err)
	return mediaInfo{
		path:     "/hls/live",
		name:     "video",
		schema:   ledger.TrackSchema{Timescale: uint32(cmaf.Timescale), TimeSource: ledger.TimeSourceIngest},
		packager: p,
	}
}

// feedCfg is the orchestration-test default: a live timeout long enough that it
// never fires while the feeder answers immediately.
func feedCfg() feedConfig { return feedConfig{liveTimeout: time.Second} }

// validGroupBodies builds n LOC frames with strictly increasing microsecond
// timestamps spaced step apart from base — the shape drainGroup and
// sampleDurations accept (two or more advancing frames).
func validGroupBodies(base, step uint64, n int) [][]byte {
	bodies := make([][]byte, n)
	for i := range n {
		bodies[i] = cmaf.EncodeLOC(base+uint64(i)*step, []byte("frame"))
	}
	return bodies
}

// feedMedia commits every group of a healthy feed and places each by the media
// time accumulated so far, so the fragments land in order without gaps.
func TestFeedMedia_AppendsEachGroupAdvancingMediaTime(t *testing.T) {
	appender := &fakeAppender{}
	sub := &fakeSubscriber{feeder: &fakeFeeder{groups: []acceptResult{
		{group: &fakeGroup{seq: 0, bodies: validGroupBodies(0, 33_333, 30)}},
		{group: &fakeGroup{seq: 1, bodies: validGroupBodies(1_000_000, 33_333, 30)}},
		{group: &fakeGroup{seq: 2, bodies: validGroupBodies(2_000_000, 33_333, 30)}},
	}, tail: errFeederDone}}

	err := feedMedia(context.Background(), sub, testMediaInfo(t), appender, &liveness{}, feedCfg())
	assert.ErrorIs(t, err, errFeederDone, "the feeder's terminal error ends the feed")

	require.Len(t, appender.appended, 3, "every valid group is committed")
	assert.Equal(t, int64(0), appender.appended[0].MediaTime, "the first group starts at the timeline origin")
	for i := 1; i < len(appender.appended); i++ {
		assert.Equal(t, appender.appended[i-1].MediaTime+appender.appended[i-1].Duration,
			appender.appended[i].MediaTime,
			"group %d picks up where group %d's media ended", i, i-1)
	}
}

// A group whose frames are not LOC is skipped, not fatal — and it advances the
// timeline by nothing, so the next group continues where the last good one left off.
func TestFeedMedia_SkipsGroupThatFailsToDecode(t *testing.T) {
	appender := &fakeAppender{}
	sub := &fakeSubscriber{feeder: &fakeFeeder{groups: []acceptResult{
		{group: &fakeGroup{seq: 0, bodies: validGroupBodies(0, 33_333, 30)}},
		{group: &fakeGroup{seq: 1, bodies: [][]byte{{}}}}, // truncated LOC → drainGroup fails
		{group: &fakeGroup{seq: 2, bodies: validGroupBodies(2_000_000, 33_333, 30)}},
	}, tail: errFeederDone}}

	err := feedMedia(context.Background(), sub, testMediaInfo(t), appender, &liveness{}, feedCfg())
	assert.ErrorIs(t, err, errFeederDone)

	require.Len(t, appender.appended, 2, "the undecodable group is skipped; the rest are committed")
	assert.Equal(t, uint64(0), appender.appended[0].ID.Sequence())
	assert.Equal(t, uint64(2), appender.appended[1].ID.Sequence())
	prev, cur := appender.appended[0], appender.appended[1]
	assert.Equal(t, prev.MediaTime+prev.Duration, cur.MediaTime,
		"a skipped group leaves no gap in the timeline")
}

// A group too small to measure (a single frame) cannot be packaged and is
// skipped; the timeline still runs on through the groups after it.
func TestFeedMedia_SkipsGroupThatFailsToPackage(t *testing.T) {
	appender := &fakeAppender{}
	sub := &fakeSubscriber{feeder: &fakeFeeder{groups: []acceptResult{
		{group: &fakeGroup{seq: 0, bodies: validGroupBodies(0, 33_333, 30)}},
		{group: &fakeGroup{seq: 1, bodies: [][]byte{cmaf.EncodeLOC(0, []byte("only"))}}}, // one frame → no durations
		{group: &fakeGroup{seq: 2, bodies: validGroupBodies(2_000_000, 33_333, 30)}},
	}, tail: errFeederDone}}

	err := feedMedia(context.Background(), sub, testMediaInfo(t), appender, &liveness{}, feedCfg())
	assert.ErrorIs(t, err, errFeederDone)

	require.Len(t, appender.appended, 2, "the single-frame group cannot be packaged and is skipped")
	prev, cur := appender.appended[0], appender.appended[1]
	assert.Equal(t, prev.MediaTime+prev.Duration, cur.MediaTime, "the skipped group leaves no gap")
}

// A group the ledger refuses (a duplicate, an ordering contradiction) is skipped
// rather than stopping the feed, and does not advance the timeline.
func TestFeedMedia_SkipsGroupTheLedgerRefuses(t *testing.T) {
	appender := &fakeAppender{errs: []error{nil, ledger.ErrGroupExists, nil}}
	sub := &fakeSubscriber{feeder: &fakeFeeder{groups: []acceptResult{
		{group: &fakeGroup{seq: 0, bodies: validGroupBodies(0, 33_333, 30)}},
		{group: &fakeGroup{seq: 1, bodies: validGroupBodies(1_000_000, 33_333, 30)}},
		{group: &fakeGroup{seq: 2, bodies: validGroupBodies(2_000_000, 33_333, 30)}},
	}, tail: errFeederDone}}

	err := feedMedia(context.Background(), sub, testMediaInfo(t), appender, &liveness{}, feedCfg())
	assert.ErrorIs(t, err, errFeederDone)

	require.Len(t, appender.appended, 2, "the ledger's refusal skips the group without stopping the feed")
	assert.Equal(t, uint64(0), appender.appended[0].ID.Sequence())
	assert.Equal(t, uint64(2), appender.appended[1].ID.Sequence())
	prev, cur := appender.appended[0], appender.appended[1]
	assert.Equal(t, prev.MediaTime+prev.Duration, cur.MediaTime,
		"a refused append does not advance the timeline")
}

// A subscribe failure ends the feed before any group is read.
func TestFeedMedia_SubscribeFailureEndsFeed(t *testing.T) {
	appender := &fakeAppender{}
	sub := &fakeSubscriber{err: errSubscribe}

	err := feedMedia(context.Background(), sub, testMediaInfo(t), appender, &liveness{}, feedCfg())
	assert.ErrorIs(t, err, errSubscribe, "a subscribe failure ends the feed immediately")
	assert.Empty(t, appender.appended)
}

// Silence past the live timeout ends the feed as the publisher leaving — the
// egress's own clock, not the relay's, decides recovery. Deterministic under
// synctest: the accept context's deadline is the only time operation in flight.
func TestFeedMedia_TreatsSilenceAsPublisherGone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		appender := &fakeAppender{}
		// Empty queue, no tail: AcceptGroup blocks until the accept context
		// expires — a publisher that went quiet without closing.
		sub := &fakeSubscriber{feeder: &fakeFeeder{}}

		err := feedMedia(context.Background(), sub, testMediaInfo(t), appender, &liveness{},
			feedConfig{liveTimeout: 100 * time.Millisecond})

		assert.ErrorContains(t, err, "treating the publisher as gone",
			"silence past the live timeout ends the feed as the publisher leaving")
		assert.Empty(t, appender.appended, "no group arrived, so nothing was committed")
	})
}
