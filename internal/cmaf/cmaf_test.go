package cmaf_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/cmaf"
)

const vp9 = "vp09.00.10.08"

func newPackager(tb testing.TB) *cmaf.Packager {
	tb.Helper()
	p, err := cmaf.NewPackager(cmaf.VideoConfig{Codec: vp9, Width: 1280, Height: 720})
	require.NoError(tb, err)
	return p
}

// frames builds a run of frames one interval apart, the first opening a group.
func frames(start, interval uint64, n int) []cmaf.Frame {
	out := make([]cmaf.Frame, n)
	for i := range n {
		out[i] = cmaf.Frame{
			Timestamp: start + uint64(i)*interval,
			Sync:      i == 0,
			Data:      bytes.Repeat([]byte{byte(i + 1)}, 16),
		}
	}
	return out
}

// boxes lists the top-level box types of a segment, in order.
func boxes(tb testing.TB, data []byte) []string {
	tb.Helper()
	var out []string
	for at := 0; at+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[at : at+4]))
		require.GreaterOrEqual(tb, size, 8, "box size at offset %d", at)
		out = append(out, string(data[at+4:at+8]))
		at += size
	}
	return out
}

// The init segment is what EXT-X-MAP points at: the codec description and
// nothing else. A media fragment in here is the bug that produced a 71 KB
// "init" the first time this pipeline was built.
func TestPackager_Init(t *testing.T) {
	init := newPackager(t).Init()

	assert.Equal(t, []string{"ftyp", "moov"}, boxes(t, init),
		"an init segment is ftyp+moov, with no fragment attached")

	// mp4ff's decoder is the independent check on what was written.
	f, err := mp4.DecodeFile(bytes.NewReader(init))
	require.NoError(t, err)
	require.NotNil(t, f.Init)
	assert.Equal(t, uint32(cmaf.Timescale), f.Init.Moov.Trak.Mdia.Mdhd.Timescale,
		"microseconds, so LOC timestamps carry with no scaling")
}

// A codec string that states no profile, level and bit depth cannot describe a
// track, and a track described wrongly plays as nothing at all.
func TestNewPackager_Rejects(t *testing.T) {
	tests := map[string]cmaf.VideoConfig{
		"no picture size":      {Codec: vp9},
		"truncated vp9 codec":  {Codec: "vp09.00", Width: 640, Height: 480},
		"unsupported codec":    {Codec: "theora", Width: 640, Height: 480},
		"avc without its sets": {Codec: "avc1.42E01E", Width: 640, Height: 480},
	}

	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := cmaf.NewPackager(cfg)
			assert.Error(t, err)
		})
	}
}

// A fragment is moof+mdat, and its extent is the sum of its samples — the value
// that becomes an EXTINF, so it has to match what the fragment actually says.
func TestPackager_Fragment(t *testing.T) {
	const interval = uint64(33_333) // ~30fps in microseconds
	p := newPackager(t)

	data, duration, err := p.Fragment(1, frames(1_000_000, interval, 30), 0, uint32(interval))
	require.NoError(t, err)

	assert.Equal(t, []string{"moof", "mdat"}, boxes(t, data))
	assert.Equal(t, uint64(30*interval), duration,
		"29 measured gaps plus the last sample's stated duration")

	// Read the duration back out of the bytes rather than trusting the return.
	traf := parseFragment(t, data).Moof.Traf
	assert.Equal(t, duration, traf.Trun.Duration(traf.Tfhd.DefaultSampleDuration), "the reported extent is the one written into the trun")
}

// Fragments are placed and numbered by the caller, not by the capture clock and
// not by a total the packager keeps itself. A second counter would drift the
// moment the two disagreed about whether a group counted — an append the ledger
// refused would advance one and not the other — and every later fragment would
// claim a position the manifest does not list it at.
func TestPackager_FragmentTimeline(t *testing.T) {
	const interval = uint64(33_333)
	p := newPackager(t)

	// Capture timestamps start far from zero, and the second group begins well
	// after the first one ended.
	first, firstExtent, err := p.Fragment(1, frames(9_489_800, interval, 10), 0, uint32(interval))
	require.NoError(t, err)
	second, _, err := p.Fragment(2, frames(30_000_000, interval, 10), firstExtent, uint32(interval))
	require.NoError(t, err)

	assert.Equal(t, uint32(1), fragmentSequence(t, first))
	assert.Equal(t, uint32(2), fragmentSequence(t, second))

	assert.Equal(t, uint64(0), decodeTime(t, first),
		"the track starts where the caller says, whatever the capture clock read")
	assert.Equal(t, firstExtent, decodeTime(t, second))

	// A group the caller did not count leaves the timeline where it was.
	third, _, err := p.Fragment(3, frames(60_000_000, interval, 10), firstExtent, uint32(interval))
	require.NoError(t, err)
	assert.Equal(t, firstExtent, decodeTime(t, third),
		"the packager keeps no total of its own to drift from the ledger's")
}

// The first sample of a group is the keyframe a player seeks to; the rest are not.
func TestPackager_SyncSample(t *testing.T) {
	p := newPackager(t)
	data, _, err := p.Fragment(1, frames(0, 33_333, 5), 0, 33_333)
	require.NoError(t, err)

	frag := parseFragment(t, data)
	samples := frag.Moof.Traf.Trun.Samples
	require.Len(t, samples, 5)

	assert.True(t, samples[0].IsSync(), "the group opens on a keyframe")
	for i, s := range samples[1:] {
		assert.False(t, s.IsSync(), "sample %d is a delta frame", i+1)
	}
}

// Timestamps arrive off the wire. A gap that runs backwards becomes an enormous
// unsigned number that truncates into a 32-bit sample duration as noise, and
// noise reads downstream exactly like a duration somebody meant — so a group
// that cannot be measured is refused rather than given an invented extent.
func TestPackager_FragmentRejectsUnmeasurable(t *testing.T) {
	tests := map[string][]cmaf.Frame{
		"no frames": nil,
		"backwards": {
			{Timestamp: 100_000, Sync: true, Data: []byte{1}},
			{Timestamp: 50_000, Data: []byte{2}},
		},
		"repeated timestamp": {
			{Timestamp: 100_000, Sync: true, Data: []byte{1}},
			{Timestamp: 100_000, Data: []byte{2}},
		},
		"implausible gap": {
			{Timestamp: 0, Sync: true, Data: []byte{1}},
			{Timestamp: 60 * uint64(cmaf.Timescale), Data: []byte{2}},
		},
	}

	for name, f := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := newPackager(t).Fragment(1, f, 0, 33_333)
			assert.Error(t, err)
		})
	}
}

// The last frame's duration is the caller's to state, and a stated one that
// could not be a frame's is refused like a measured one.
func TestPackager_FragmentLastDuration(t *testing.T) {
	tests := map[string]struct {
		last    uint32
		wantErr bool
	}{
		"stated":      {last: 40_000},
		"none":        {last: 0, wantErr: true},
		"implausible": {last: 11 * cmaf.Timescale, wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			data, duration, err := newPackager(t).Fragment(1, frames(0, 33_333, 3), 0, tt.last)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, uint64(2*33_333)+uint64(tt.last), duration)
			traf := parseFragment(t, data).Moof.Traf
			assert.Equal(t, duration, traf.Trun.Duration(traf.Tfhd.DefaultSampleDuration))
		})
	}
}

// A single frame packages once its duration is stated: there is no gap to
// measure, but nothing left unknown either.
func TestPackager_FragmentSingleFrame(t *testing.T) {
	_, duration, err := newPackager(t).Fragment(1, frames(500, 33_333, 1), 0, 33_333)

	require.NoError(t, err)
	assert.Equal(t, uint64(33_333), duration)
}

func TestMeanInterval(t *testing.T) {
	tests := map[string]struct {
		frames  []cmaf.Frame
		want    uint32
		wantErr bool
	}{
		"steady": {frames: frames(0, 33_333, 30), want: 33_333},
		"uneven": {
			frames: []cmaf.Frame{{Timestamp: 0}, {Timestamp: 30_000}, {Timestamp: 70_000}},
			want:   35_000,
		},
		"one frame": {frames: frames(0, 33_333, 1), wantErr: true},
		"backwards": {
			frames:  []cmaf.Frame{{Timestamp: 100_000}, {Timestamp: 50_000}},
			wantErr: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := cmaf.MeanInterval(tt.frames)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func parseFragment(tb testing.TB, data []byte) *mp4.Fragment {
	tb.Helper()
	f, err := mp4.DecodeFile(bytes.NewReader(data))
	require.NoError(tb, err)
	require.Len(tb, f.Segments, 1)
	require.Len(tb, f.Segments[0].Fragments, 1)
	return f.Segments[0].Fragments[0]
}

func fragmentSequence(tb testing.TB, data []byte) uint32 {
	tb.Helper()
	return parseFragment(tb, data).Moof.Mfhd.SequenceNumber
}

func decodeTime(tb testing.TB, data []byte) uint64 {
	tb.Helper()
	return parseFragment(tb, data).Moof.Traf.Tfdt.BaseMediaDecodeTime()
}

// A stored group's LOC bodies decode into the frames the packager takes: their
// timestamps and payloads in order, the first the sync sample.
func TestFramesFromLOC(t *testing.T) {
	frames, err := cmaf.FramesFromLOC([][]byte{
		cmaf.EncodeLOC(1_000, []byte("key")),
		cmaf.EncodeLOC(34_333, []byte("delta")),
	})

	require.NoError(t, err)
	assert.Equal(t, []cmaf.Frame{
		{Timestamp: 1_000, Sync: true, Data: []byte("key")},
		{Timestamp: 34_333, Data: []byte("delta")},
	}, frames)

	_, err = cmaf.FramesFromLOC([][]byte{cmaf.EncodeLOC(0, nil), {0x40}})
	assert.ErrorContains(t, err, "frame 1", "the failing frame is named")
}
