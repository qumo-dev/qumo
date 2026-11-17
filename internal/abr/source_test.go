package abr

import (
	"bytes"
	"context"
	"encoding/binary"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/gomoqt/msf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/ingest"
)

// parameterSets returns the SPS and PPS of an AVCDecoderConfigurationRecord.
func parameterSets(t *testing.T, record []byte) (sps, pps []byte) {
	t.Helper()
	cfg, err := ingest.ParseAVCConfig(append([]byte{0x17, 0, 0, 0, 0}, record...))
	require.NoError(t, err)
	return cfg.SPS[0], cfg.PPS[0]
}

// annexB joins nalus into an Annex B byte stream with 4-byte start codes.
func annexB(nalus ...[]byte) []byte {
	var b []byte
	for _, n := range nalus {
		b = append(b, 0, 0, 0, 1)
		b = append(b, n...)
	}
	return b
}

// annexBFromAVCC rewrites an AVCC frame (4-byte lengths) as Annex B, led by
// an access unit delimiter and, for a keyframe, the parameter sets, as a
// WebCodecs encoder in "annexb" format writes it.
func annexBFromAVCC(t *testing.T, data []byte, key bool, sps, pps []byte) []byte {
	t.Helper()
	nalus := [][]byte{{0x09, 0xf0}} // AUD
	if key {
		nalus = append(nalus, sps, pps)
	}
	for len(data) > 0 {
		require.GreaterOrEqual(t, len(data), 4)
		n := int(binary.BigEndian.Uint32(data))
		nalus = append(nalus, data[4:4+n])
		data = data[4+n:]
	}
	return annexB(nalus...)
}

func TestSplitAnnexB(t *testing.T) {
	a, b, c := []byte{0x67, 1, 2}, []byte{0x68, 3}, []byte{0x65, 4, 5, 6}
	tests := map[string]struct {
		in   []byte
		want [][]byte
	}{
		"4-byte start codes": {in: annexB(a, b, c), want: [][]byte{a, b, c}},
		"3-byte start codes": {in: append(append([]byte{0, 0, 1}, a...), append([]byte{0, 0, 1}, b...)...), want: [][]byte{a, b}},
		"mixed, with trailing zero bytes": {
			in:   append(append(append([]byte{0, 0, 0, 1}, a...), 0, 0), append([]byte{0, 0, 1}, c...)...),
			want: [][]byte{a, c},
		},
		"no start code":    {in: []byte{1, 2, 3}, want: nil},
		"an empty unit":    {in: []byte{0, 0, 1, 0, 0, 1, 0x65, 9}, want: [][]byte{{0x65, 9}}},
		"too short to say": {in: []byte{0, 0}, want: nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, splitAnnexB(tt.in))
		})
	}
}

func TestAVCC(t *testing.T) {
	got := avcc([][]byte{{0x09, 0xf0}, {0x67, 1}, {0x65, 2, 3}})
	assert.Equal(t, []byte{0, 0, 0, 2, 0x67, 1, 0, 0, 0, 3, 0x65, 2, 3}, got, "access unit delimiters are left out")
}

func TestHasStartCode(t *testing.T) {
	assert.True(t, hasStartCode([]byte{0, 0, 1, 0x65}))
	assert.True(t, hasStartCode([]byte{0, 0, 0, 1, 0x65}))
	assert.False(t, hasStartCode([]byte{0, 0, 0, 5, 0x65}), "an AVCC length")
	assert.False(t, hasStartCode([]byte{0, 0}))
}

func TestAVCRecord(t *testing.T) {
	sps, pps := parameterSets(t, avcRecord360p)
	record, err := avcRecord([][]byte{{0x09, 0xf0}, sps, pps, {0x65, 1}})
	require.NoError(t, err)
	cfg, err := ingest.ParseAVCConfig(append([]byte{0x17, 0, 0, 0, 0}, record...))
	require.NoError(t, err)
	assert.Equal(t, 640, cfg.Width)
	assert.Equal(t, 360, cfg.Height)
	assert.Equal(t, "avc1.42c01e", cfg.CodecString())
	assert.Equal(t, 4, cfg.NALULenSize)

	_, err = avcRecord([][]byte{{0x65, 1}})
	assert.Error(t, err, "a frame without parameter sets")
	_, err = avcRecord([][]byte{sps})
	assert.Error(t, err, "an SPS without a PPS")
}

func TestFormatOf(t *testing.T) {
	size := func(t msf.Track) msf.Track {
		t.Width, t.Height = new(int64(640)), new(int64(360))
		return t
	}
	tests := map[string]struct {
		track  msf.Track
		init   []byte
		want   sourceFormat
		wantOK bool
	}{
		"H.264 with init data is AVCC":    {track: msf.Track{Codec: "avc1.64001f"}, init: []byte{1}, want: formatAVCC, wantOK: true},
		"H.264 without init is Annex B":   {track: msf.Track{Codec: "avc1.64001f"}, want: formatAnnexB, wantOK: true},
		"VP9 with its size":               {track: size(msf.Track{Codec: "vp09.00.10.08"}), want: formatVP9, wantOK: true},
		"VP9 without a size":              {track: msf.Track{Codec: "vp09.00.10.08"}},
		"VP9 without profile and level":   {track: size(msf.Track{Codec: "vp09"})},
		"AV1 is not taken":                {track: size(msf.Track{Codec: "av01.0.08M.08"})},
		"avc3 carries its sets in band":   {track: msf.Track{Codec: "avc3.64001f"}},
		"a track with no codec":           {track: msf.Track{}},
		"VP8 states no configuration":     {track: size(msf.Track{Codec: "vp8"})},
		"HEVC is not taken":               {track: size(msf.Track{Codec: "hev1.1.6.L93.B0"})},
		"VP9 with zero size is not taken": {track: msf.Track{Codec: "vp09.00.10.08", Width: new(int64(0)), Height: new(int64(0))}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := formatOf(tt.track, tt.init)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestSourceAnnexB(t *testing.T) {
	sps, pps := parameterSets(t, avcRecord360p)
	s, err := newSource(msf.Track{Codec: "avc1.42c01e"}, nil, formatAnnexB)
	require.NoError(t, err)
	assert.Nil(t, s.config, "known only from the stream")

	// Joined mid-group: nothing is decodable before a keyframe.
	_, ok := s.frame(annexB([]byte{0x41, 1}), false)
	assert.False(t, ok)
	// A keyframe without its parameter sets does not make it known either.
	_, ok = s.frame(annexB([]byte{0x65, 1}), true)
	assert.False(t, ok)

	got, ok := s.frame(annexB([]byte{0x09, 0xf0}, sps, pps, []byte{0x65, 7}), true)
	require.True(t, ok)
	require.NotNil(t, s.config)
	assert.Equal(t, 640, s.config.Width)
	assert.Equal(t, 360, s.config.Height)
	assert.Equal(t, avcc([][]byte{sps, pps, {0x65, 7}}), got, "rewritten as AVCC, without the delimiter")

	got, ok = s.frame(annexB([]byte{0x41, 8}), false)
	require.True(t, ok)
	assert.Equal(t, []byte{0, 0, 0, 2, 0x41, 8}, got)

	// AVCC sent without a configuration cannot be decoded.
	_, ok = s.frame([]byte{0, 0, 0, 2, 0x41, 8}, false)
	assert.False(t, ok)
}

func TestSourceAVCCAndVP9(t *testing.T) {
	s, err := newSource(msf.Track{Codec: "avc1.42c01e"}, avcRecord360p, formatAVCC)
	require.NoError(t, err)
	require.NotNil(t, s.config)
	assert.Equal(t, 640, s.config.Width)
	got, ok := s.frame([]byte{0, 0, 0, 1, 0x41}, false)
	assert.True(t, ok)
	assert.Equal(t, []byte{0, 0, 0, 1, 0x41}, got, "passed as is")

	_, err = newSource(msf.Track{Codec: "avc1.42c01e"}, []byte{1, 2}, formatAVCC)
	assert.Error(t, err, "init data that does not parse")

	vp9 := msf.Track{Codec: "vp09.00.10.08", Width: new(int64(1280)), Height: new(int64(720))}
	s, err = newSource(vp9, nil, formatVP9)
	require.NoError(t, err)
	assert.Equal(t, sourceConfig{Codec: "vp09.00.10.08", Width: 1280, Height: 720}, *s.config)
	got, ok = s.frame([]byte{0x82, 0x49}, true)
	assert.True(t, ok)
	assert.Equal(t, []byte{0x82, 0x49}, got)
}

// encodeAll feeds src through an encoder for outputs and returns what the
// sink recorded.
func encodeAll(t *testing.T, sc sourceConfig, src []sourceFrame, outputs []output) *recordingSink {
	t.Helper()
	sink := newRecordingSink()
	enc, err := newEncoder(EncoderConfig{FFmpeg: "ffmpeg", Codec: "libx264", Preset: "ultrafast"}, sc, outputs, sink)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	require.NoError(t, enc.start(ctx))
	for _, f := range src {
		require.NoError(t, enc.encode(f))
	}
	require.NoError(t, enc.close())
	return sink
}

// assertRenditions checks that every source frame came out of every rendition,
// with its timestamps, and that every group opens on a keyframe.
func assertRenditions(t *testing.T, sink *recordingSink, src []sourceFrame, outputs []output) {
	t.Helper()
	assert.False(t, sink.early)
	for i, o := range outputs {
		got := sink.frames[i]
		require.Len(t, got, len(src), o.Rung.TrackName())
		for k, f := range got {
			assert.Equal(t, src[k].frameInfo, f.frameInfo, "%s frame %d", o.Rung.TrackName(), k)
			if f.First {
				assert.True(t, f.Keyframe, "%s frame %d", o.Rung.TrackName(), k)
			}
		}
	}
}

func TestEncoderAnnexBSource(t *testing.T) {
	record, frames := testSource(t, 640, 360, 60, 30)
	sps, pps := parameterSets(t, record)
	s, err := newSource(msf.Track{Codec: "avc1.42c01e"}, nil, formatAnnexB)
	require.NoError(t, err)

	var src []sourceFrame
	for _, f := range frames {
		data, ok := s.frame(annexBFromAVCC(t, f.Data, f.First, sps, pps), f.First)
		require.True(t, ok)
		f.Data = data
		src = append(src, f)
	}
	outputs := []output{{Rung: Rung{Height: 240, Bitrate: 400_000}, Width: 426, Height: 240}}
	assertRenditions(t, encodeAll(t, *s.config, src, outputs), src, outputs)
}

// testVP9Source encodes a synthetic VP9 source with ffmpeg and returns its
// frames, grouped at keyframes, read from an IVF stream.
func testVP9Source(t *testing.T, width, height, frames, gop int) []sourceFrame {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size="+strconv.Itoa(width)+"x"+strconv.Itoa(height)+":rate=30",
		"-frames:v", strconv.Itoa(frames), "-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8",
		"-g", strconv.Itoa(gop), "-keyint_min", strconv.Itoa(gop), "-pix_fmt", "yuv420p", "-f", "ivf", "pipe:1").Output()
	if err != nil {
		t.Skipf("ffmpeg cannot encode VP9: %v", err)
	}
	// IVF: a 32-byte file header, then frames of a 12-byte header (size,
	// timestamp) and data. A VP9 keyframe has frame_type 0, bit 2 (profile
	// 0) of its first byte.
	require.Equal(t, "DKIF", string(out[:4]))
	r := bytes.NewReader(out[32:])
	var src []sourceFrame
	var group moqt.GroupSequence
	hdr := make([]byte, 12)
	for {
		if _, err := r.Read(hdr); err != nil {
			break
		}
		data := make([]byte, binary.LittleEndian.Uint32(hdr))
		_, err := r.Read(data)
		require.NoError(t, err)
		key := data[0]&0x04 == 0
		if key {
			group++
		}
		ts := uint64(1_794_839_947_021_017 + len(src)*33_367)
		src = append(src, sourceFrame{frameInfo: frameInfo{Group: group, First: key, Timestamp: ts, WireTimestamp: ts}, Data: data})
	}
	require.Len(t, src, frames)
	return src
}

func TestEncoderVP9Source(t *testing.T) {
	src := testVP9Source(t, 640, 360, 60, 30)
	keys := 0
	for _, f := range src {
		if f.First {
			keys++
		}
	}
	require.Equal(t, 2, keys, "a keyframe every 30 frames")
	s, err := newSource(msf.Track{Codec: "vp09.00.10.08", Width: new(int64(640)), Height: new(int64(360))}, nil, formatVP9)
	require.NoError(t, err)
	outputs := []output{{Rung: Rung{Height: 240, Bitrate: 400_000}, Width: 426, Height: 240}}
	sink := encodeAll(t, *s.config, src, outputs)
	assertRenditions(t, sink, src, outputs)

	cfg, err := ingest.ParseAVCConfig(append([]byte{0x17, 0, 0, 0, 0}, sink.configs[0]...))
	require.NoError(t, err)
	assert.Contains(t, cfg.CodecString(), "avc1.", "the renditions are H.264, whatever the source")
}
