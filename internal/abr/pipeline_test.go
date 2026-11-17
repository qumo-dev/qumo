package abr

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/gomoqt/msf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/cmaf"
)

// avcRecord360p is the AVCDecoderConfigurationRecord libx264 writes for a
// 640×360 constrained-baseline stream (profile 0x42, level 3.0).
var avcRecord360p = mustHex("0142c01effe100196742c01eda0280bfe5c044000003000400000300f03c58ba8001000468ce0fc8")

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func TestInitData(t *testing.T) {
	data := base64.StdEncoding.EncodeToString([]byte{1, 2, 3})
	catalog := msf.Catalog{InitDataList: []msf.InitDataRef{
		{ID: "video", Type: "inline", Data: data},
		{ID: "untyped", Data: data},
		{ID: "remote", Type: "url", Data: "https://example.com/init"},
		{ID: "garbled", Type: "inline", Data: "not base64!"},
	}}
	tests := map[string]struct {
		ref     string
		want    []byte
		wantErr bool
	}{
		"inline":                   {ref: "video", want: []byte{1, 2, 3}},
		"no type counts as inline": {ref: "untyped", want: []byte{1, 2, 3}},
		"no reference":             {ref: "", want: nil},
		"missing entry":            {ref: "audio", wantErr: true},
		"not inline":               {ref: "remote", wantErr: true},
		"bad base64":               {ref: "garbled", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := initData(catalog, msf.Track{Name: "t", InitRef: tt.ref})
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIsAVC(t *testing.T) {
	assert.True(t, isAVC("avc1.64001f"))
	// avc3 carries its parameter sets in band, which the fMP4 input to
	// ffmpeg is not built for.
	assert.False(t, isAVC("avc3.64001f"))
	assert.False(t, isAVC("vp09.00.10.08"))
	assert.False(t, isAVC(""))
}

func TestOutputTrack(t *testing.T) {
	got := outputTrack(msf.Track{Name: "video", Namespace: "/live/cam1", InitRef: "v", Codec: "avc1.64001f"})
	assert.Empty(t, got.Namespace, "in the output broadcast's own namespace")
	assert.Empty(t, got.InitRef, "addTrack lists the init data afresh")
	assert.Equal(t, "avc1.64001f", got.Codec)
}

// The source rendition is sized by its catalog when that states a positive
// width and height, else by its decoded configuration, and states no size
// (nor a label from one) when neither is known: never 0p.
func TestSourceRendition(t *testing.T) {
	decoded := &sourceConfig{Width: 640, Height: 360}
	tests := map[string]struct {
		width, height *int64
		label         string
		config        *sourceConfig
		wantSize      []int64 // width, height; nil for none
		wantLabel     string
	}{
		"catalog size":                     {width: new(int64(1280)), height: new(int64(720)), config: decoded, wantSize: []int64{1280, 720}, wantLabel: "720p (source)"},
		"catalog size, not decoded yet":    {width: new(int64(1280)), height: new(int64(720)), wantSize: []int64{1280, 720}, wantLabel: "720p (source)"},
		"catalog label kept":               {width: new(int64(1280)), height: new(int64(720)), label: "HD", wantSize: []int64{1280, 720}, wantLabel: "HD"},
		"no catalog size, decoded":         {config: decoded, wantSize: []int64{640, 360}, wantLabel: "360p (source)"},
		"no catalog size, not decoded yet": {},
		"no catalog height, decoded":       {width: new(int64(1280)), config: decoded, wantSize: []int64{640, 360}, wantLabel: "360p (source)"},
		"zero width, decoded":              {width: new(int64(0)), height: new(int64(720)), config: decoded, wantSize: []int64{640, 360}, wantLabel: "360p (source)"},
		"zero height, decoded":             {width: new(int64(1280)), height: new(int64(0)), config: decoded, wantSize: []int64{640, 360}, wantLabel: "360p (source)"},
		"zero size, decoded":               {width: new(int64(0)), height: new(int64(0)), config: decoded, wantSize: []int64{640, 360}, wantLabel: "360p (source)"},
		"zero size, not decoded yet":       {width: new(int64(0)), height: new(int64(0))},
		"zero size, catalog label kept":    {width: new(int64(0)), height: new(int64(0)), label: "cam", wantLabel: "cam"},
		"negative size, decoded":           {width: new(int64(-1)), height: new(int64(-1)), config: decoded, wantSize: []int64{640, 360}, wantLabel: "360p (source)"},
		"decoded size unusable":            {config: &sourceConfig{}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			track := msf.Track{Name: "video", Namespace: "/live/cam1", Codec: "avc1.42c01e", Width: tt.width, Height: tt.height, Label: tt.label}
			got := sourceRendition(track, tt.config)
			require.NotNil(t, got.AltGroup)
			assert.EqualValues(t, videoAltGroup, *got.AltGroup)
			assert.Empty(t, got.Namespace)
			assert.Equal(t, tt.wantLabel, got.Label)
			if tt.wantSize == nil {
				assert.Nil(t, got.Width)
				assert.Nil(t, got.Height)
				return
			}
			require.NotNil(t, got.Width)
			require.NotNil(t, got.Height)
			assert.Equal(t, tt.wantSize, []int64{*got.Width, *got.Height})
		})
	}
}

func TestMisdirectedError(t *testing.T) {
	err := &MisdirectedError{OptIn: "/.abr/live/cam1", Broadcast: "/live/cam1", To: "/live/other"}
	assert.Equal(t, "abr: the opt-in /.abr/live/cam1 leads to /live/other, not /live/cam1", err.Error())
}

// newTestRenditions returns renditions publishing into a fresh output
// broadcast: 360p and 240p, at 800 and 400 kbit/s.
func newTestRenditions(t *testing.T) *renditions {
	t.Helper()
	out, err := newOutputBroadcast()
	require.NoError(t, err)
	t.Cleanup(out.close)
	outputs := []output{
		{Rung: Rung{Height: 360, Bitrate: 800_000}, Width: 640, Height: 360},
		{Rung: Rung{Height: 240, Bitrate: 400_000}, Width: 426, Height: 240},
	}
	r := &renditions{out: out, outputs: outputs, info: defaultVideoInfo,
		tracks: []*track{newTrack(), newTrack()}, configured: make([]bool, 2)}
	return r
}

func TestRenditionsConfigure(t *testing.T) {
	r := newTestRenditions(t)
	r.Configure(0, avcRecord360p)

	got := catalogTrack(t, r.out, "video-360p")
	require.NotNil(t, got.AltGroup)
	assert.EqualValues(t, videoAltGroup, *got.AltGroup, "an alternative of the source")
	assert.Equal(t, "avc1.42c01e", got.Codec, "from the encoder's configuration")
	assert.EqualValues(t, 640, *got.Width)
	assert.EqualValues(t, 360, *got.Height)
	assert.EqualValues(t, 800_000, *got.Bitrate)
	assert.Equal(t, "360p", got.Label)
	assert.Equal(t, "video-360p", got.InitRef)

	// A configuration that does not parse adds nothing.
	r.Configure(1, []byte{1, 2})
	for _, tr := range r.out.b.Catalog().Tracks {
		assert.NotEqual(t, "video-240p", tr.Name)
	}
}

func TestRenditionsWriteFrame(t *testing.T) {
	frame := func(group moqt.GroupSequence, first, key bool, ts uint64) renditionFrame {
		return renditionFrame{
			frameInfo: frameInfo{Group: group, First: first, Timestamp: ts, WireTimestamp: ts + 1},
			Keyframe:  key,
			Data:      []byte{byte(ts)},
		}
	}
	t.Run("dropped before the rendition is configured", func(t *testing.T) {
		r := newTestRenditions(t)
		r.WriteFrame(0, frame(5, true, true, 100))
		assert.False(t, r.tracks[0].next(position{}).found)
	})

	t.Run("groups follow the source's, opening at its keyframes", func(t *testing.T) {
		r := newTestRenditions(t)
		r.Configure(0, avcRecord360p)
		// The encoder joined mid-group: frames before a group opens are
		// not decodable, and are refused.
		r.WriteFrame(0, frame(4, false, false, 90))
		assert.False(t, r.tracks[0].next(position{}).found)

		r.WriteFrame(0, frame(5, true, true, 100))
		r.WriteFrame(0, frame(5, false, false, 133))
		v := r.tracks[0].next(position{})
		require.True(t, v.found)
		assert.Equal(t, moqt.GroupSequence(5), v.seq, "numbered as the source's group")
		require.Len(t, v.frames, 2)

		ts, data, err := cmaf.DecodeLOC(v.frames[0].Body())
		require.NoError(t, err)
		assert.Equal(t, uint64(100), ts, "the source frame's LOC timestamp")
		assert.Equal(t, []byte{100}, data)
		assert.Equal(t, uint64(101), v.frames[0].Timestamp, "and its wire timestamp")

		r.WriteFrame(0, frame(6, true, true, 166))
		v = r.tracks[0].next(position{served: 5, anyDone: true})
		assert.Equal(t, moqt.GroupSequence(6), v.seq)
	})

	t.Run("a group opening on a non-keyframe continues the previous one", func(t *testing.T) {
		r := newTestRenditions(t)
		r.Configure(1, avcRecord360p)
		r.WriteFrame(1, frame(5, true, true, 100))
		r.WriteFrame(1, frame(6, true, false, 133)) // not a decode point
		v := r.tracks[1].next(position{})
		assert.Equal(t, moqt.GroupSequence(5), v.seq)
		assert.Len(t, v.frames, 2, "a joiner at group 6 could not decode it")
		assert.False(t, v.done)
	})
}

// When a later group cannot start, the encoders already started are closed:
// their ffmpeg is told its input ended and is waited for, not left running.
// The ffmpeg is the test binary (TestMain), which writes a marker once its
// input ends; ctx is never cancelled, so nothing else would end it.
func TestStartEncodersClosesStartedOnFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(fakeFFmpegDir, dir)
	self, err := os.Executable()
	require.NoError(t, err)
	r := newTestRenditions(t)
	src := sourceConfig{Codec: "avc1.42c01e", Width: 640, Height: 360, Description: avcRecord360p}

	// The first group starts an ffmpeg; the second has no outputs, so it
	// cannot start.
	encoders, err := startEncoders(context.Background(), EncoderConfig{FFmpeg: self, Codec: "libx264"},
		src, r.outputs[:1], [][]int{{0}, {}}, r)

	assert.Error(t, err)
	assert.Nil(t, encoders)
	assert.FileExists(t, filepath.Join(dir, fakeFFmpegMarker), "the started ffmpeg was closed and waited for")
}

func TestStartEncodersMissingFFmpeg(t *testing.T) {
	r := newTestRenditions(t)
	src := sourceConfig{Codec: "avc1.42c01e", Width: 640, Height: 360, Description: avcRecord360p}
	_, err := startEncoders(context.Background(), EncoderConfig{FFmpeg: "/nonexistent/ffmpeg", Codec: "libx264"},
		src, r.outputs, [][]int{{0, 1}}, r)
	assert.Error(t, err)
}
