package abr

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"testing"
	"time"

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

func TestStartEncodersClosesStartedOnFailure(t *testing.T) {
	record, _ := testSource(t, 320, 240, 1, 1)
	r := newTestRenditions(t)
	src := sourceConfig{Codec: "avc1.42c00d", Width: 320, Height: 240, Description: record}
	outs := []output{r.outputs[0]}
	// The first group starts an ffmpeg; the second has no outputs, so it
	// cannot, and the first must be stopped and waited for.
	done := make(chan error, 1)
	go func() {
		encoders, err := startEncoders(t.Context(), EncoderConfig{FFmpeg: "ffmpeg", Codec: "libx264", Preset: "ultrafast"},
			src, outs, [][]int{{0}, {}}, r)
		assert.Nil(t, encoders)
		done <- err
	}()
	select {
	case err := <-done:
		assert.Error(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("startEncoders did not return: the started ffmpeg was not stopped")
	}
}

func TestStartEncodersMissingFFmpeg(t *testing.T) {
	r := newTestRenditions(t)
	src := sourceConfig{Codec: "avc1.42c01e", Width: 640, Height: 360, Description: avcRecord360p}
	_, err := startEncoders(context.Background(), EncoderConfig{FFmpeg: "/nonexistent/ffmpeg", Codec: "libx264"},
		src, r.outputs, [][]int{{0, 1}}, r)
	assert.Error(t, err)
}
