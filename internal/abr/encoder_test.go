package abr

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/ingest"
)

// recordingSink records what an encoder outputs.
type recordingSink struct {
	mu      sync.Mutex
	configs map[int][]byte
	frames  map[int][]renditionFrame
	early   bool // a frame arrived before its rendition's configuration
}

func newRecordingSink() *recordingSink {
	return &recordingSink{configs: map[int][]byte{}, frames: map[int][]renditionFrame{}}
}

func (s *recordingSink) Configure(i int, record []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configs[i] = bytes.Clone(record)
}

func (s *recordingSink) WriteFrame(i int, f renditionFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.configs[i] == nil {
		s.early = true
	}
	s.frames[i] = append(s.frames[i], f)
}

// testSource encodes a synthetic H.264 source with ffmpeg and returns its
// decoder configuration record and frames, grouped at keyframes and stamped
// about 30 fps apart, in microseconds off any frame-rate grid and not whole
// milliseconds, as a live source's are.
func testSource(t *testing.T, width, height, frames, gop int) ([]byte, []sourceFrame) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size="+strconv.Itoa(width)+"x"+strconv.Itoa(height)+":rate=30",
		"-frames:v", strconv.Itoa(frames), "-c:v", "libx264", "-preset", "ultrafast", "-bf", "0",
		"-g", strconv.Itoa(gop), "-sc_threshold", "0", "-pix_fmt", "yuv420p", "-f", "flv", "pipe:1").Output()
	require.NoError(t, err)

	fr := newFLVReader(bytes.NewReader(out))
	var record []byte
	var src []sourceFrame
	var group moqt.GroupSequence
	for {
		tag, err := fr.Next()
		if err != nil {
			break
		}
		switch {
		case tag.isSequenceHeader():
			record = bytes.Clone(tag.payload())
		case tag.isFrame():
			first := tag.isKeyframe()
			if first {
				group++
			}
			ts := uint64(5_021_017 + len(src)*33_367)
			src = append(src, sourceFrame{
				frameInfo: frameInfo{Group: group, First: first, Timestamp: ts, WireTimestamp: ts},
				Data:      bytes.Clone(tag.payload()),
			})
		}
	}
	require.NotNil(t, record)
	require.Len(t, src, frames)
	return record, src
}

func TestEncoderRenditionsMatchSource(t *testing.T) {
	record, src := testSource(t, 1280, 720, 90, 30)
	cfg, err := ingest.ParseAVCConfig(append([]byte{0x17, 0, 0, 0, 0}, record...))
	require.NoError(t, err)

	outputs := []output{{Rung: Rung{Height: 360, Bitrate: 800_000}}, {Rung: Rung{Height: 240, Bitrate: 400_000}}}
	for i := range outputs {
		outputs[i].Width, outputs[i].Height = outputs[i].Rung.Size(1280, 720)
	}
	sink := newRecordingSink()
	enc, err := newEncoder(EncoderConfig{FFmpeg: "ffmpeg", Codec: "libx264", Preset: "ultrafast"},
		sourceConfig{Codec: cfg.CodecString(), Width: cfg.Width, Height: cfg.Height, Description: record},
		outputs, sink)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	require.NoError(t, enc.start(ctx))
	for _, f := range src {
		require.NoError(t, enc.encode(f))
	}
	require.NoError(t, enc.close())

	assert.False(t, sink.early, "a frame arrived before its rendition's configuration")
	for i, o := range outputs {
		got := sink.frames[i]
		require.Len(t, got, len(src), o.Rung.TrackName())

		oc, err := ingest.ParseAVCConfig(append([]byte{0x17, 0, 0, 0, 0}, sink.configs[i]...))
		require.NoError(t, err)
		assert.Equal(t, o.Width, oc.Width, o.Rung.TrackName())
		assert.Equal(t, o.Height, oc.Height, o.Rung.TrackName())

		for k, f := range got {
			assert.Equal(t, src[k].frameInfo, f.frameInfo, "%s frame %d", o.Rung.TrackName(), k)
			if f.First {
				assert.True(t, f.Keyframe, "%s frame %d opens group %d but is not a keyframe", o.Rung.TrackName(), k, f.Group)
			}
		}
	}
}
