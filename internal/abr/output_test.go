package abr

import (
	"encoding/base64"
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/gomoqt/msf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func catalogTrack(t *testing.T, o *outputBroadcast, name string) msf.Track {
	t.Helper()
	for _, tr := range o.b.Catalog().Tracks {
		if tr.Name == name {
			return tr
		}
	}
	t.Fatalf("catalog has no track %s", name)
	return msf.Track{}
}

func TestOutputBroadcastAddTrack(t *testing.T) {
	o, err := newOutputBroadcast()
	require.NoError(t, err)
	defer o.close()

	video := msf.Track{Name: "video", Packaging: msf.PackagingLOC, Role: msf.RoleVideo, IsLive: new(true), Codec: "avc1.64001f"}
	require.NoError(t, o.addTrack(video, []byte{1, 2, 3}, defaultVideoInfo, newTrack()))

	got := catalogTrack(t, o, "video")
	assert.Equal(t, "video", got.InitRef, "init data is listed under the track's name")
	list := o.b.Catalog().InitDataList
	require.Len(t, list, 1)
	assert.Equal(t, "video", list[0].ID)
	assert.Equal(t, "inline", list[0].Type)
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte{1, 2, 3}), list[0].Data)

	// Adding it again, with a new configuration, replaces both.
	require.NoError(t, o.addTrack(video, []byte{4, 5}, defaultVideoInfo, newTrack()))
	list = o.b.Catalog().InitDataList
	require.Len(t, list, 1, "replaced, not listed twice")
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte{4, 5}), list[0].Data)
	assert.Len(t, o.b.Catalog().Tracks, 1)

	// A track without init data references none.
	audio := msf.Track{Name: "audio", Packaging: msf.PackagingLOC, Role: msf.RoleAudio, IsLive: new(true), Codec: "opus"}
	require.NoError(t, o.addTrack(audio, nil, defaultAudioInfo, newTrack()))
	assert.Empty(t, catalogTrack(t, o, "audio").InitRef)
	assert.Len(t, o.b.Catalog().InitDataList, 1)
}

func TestOutputBroadcastTrackInfo(t *testing.T) {
	o, err := newOutputBroadcast()
	require.NoError(t, err)
	defer o.close()
	info := moqt.PublishInfo{Priority: 7, Ordered: true, MaxLatency: 1500, Timescale: 90_000}
	require.NoError(t, o.addTrack(msf.Track{Name: "video", Packaging: msf.PackagingLOC, Role: msf.RoleVideo, IsLive: new(true)}, nil, info, newTrack()))

	got, ok := o.TrackInfo("video")
	assert.True(t, ok)
	assert.Equal(t, info, got, "the source's publisher properties are passed on")

	got, ok = o.TrackInfo(o.b.CatalogTrackName())
	assert.True(t, ok)
	assert.Equal(t, catalogInfo, got)

	_, ok = o.TrackInfo("nope")
	assert.False(t, ok)
}

func TestOutputBroadcastSetBitrate(t *testing.T) {
	o, err := newOutputBroadcast()
	require.NoError(t, err)
	defer o.close()
	require.NoError(t, o.addTrack(msf.Track{Name: "video", Packaging: msf.PackagingLOC, Role: msf.RoleVideo, IsLive: new(true)}, nil, defaultVideoInfo, newTrack()))

	require.NoError(t, o.setBitrate("video", 5_000_000))
	got := catalogTrack(t, o, "video")
	require.NotNil(t, got.Bitrate)
	assert.EqualValues(t, 5_000_000, *got.Bitrate)

	assert.Error(t, o.setBitrate("nope", 1))
}
