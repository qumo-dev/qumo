package fmp4_test

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/fmp4"
)

// box builds an ISO-BMFF box: a 32-bit size, a 4-character type, then payload.
func box(typ string, payload ...[]byte) []byte {
	var body []byte
	for _, p := range payload {
		body = append(body, p...)
	}
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(body)))
	copy(out[4:8], typ)
	return append(out, body...)
}

func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

func u64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

// mdhdV0 is a version-0 media header carrying timescale.
func mdhdV0(timescale uint32) []byte {
	return box("mdhd",
		[]byte{0, 0, 0, 0}, // version 0, flags
		u32(0), u32(0),     // creation, modification
		u32(timescale),
		u32(0), // duration
	)
}

// mdhdV1 is the 64-bit variant, where timescale sits 8 bytes further in.
func mdhdV1(timescale uint32) []byte {
	return box("mdhd",
		[]byte{1, 0, 0, 0}, // version 1, flags
		u64(0), u64(0),     // creation, modification
		u32(timescale),
		u64(0), // duration
	)
}

func initSegment(mdhd []byte) []byte {
	return append(
		box("ftyp", []byte("iso5")),
		box("moov", box("trak", box("mdia", mdhd)))...,
	)
}

func TestTimescale(t *testing.T) {
	tests := map[string]struct {
		init     []byte
		expected uint32
		wantErr  bool
	}{
		"version 0":      {init: initSegment(mdhdV0(57600)), expected: 57600},
		"version 1":      {init: initSegment(mdhdV1(90000)), expected: 90000},
		"no mdhd":        {init: box("moov", box("trak")), wantErr: true},
		"empty":          {init: nil, wantErr: true},
		"mdhd truncated": {init: initSegment(box("mdhd", []byte{0, 0, 0, 0})), wantErr: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := fmp4.Timescale(tt.init)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}

// trak builds a track with the given track_ID and media timescale.
func trak(trackID, timescale uint32) []byte {
	tkhd := box("tkhd",
		[]byte{0, 0, 0, 0}, // version 0, flags
		u32(0), u32(0),     // creation, modification
		u32(trackID),
	)
	return box("trak", tkhd, box("mdia", mdhdV0(timescale)))
}

// An init describing several tracks has no single timescale, and picking one
// would silently scale every duration the caller computes by the ratio between
// two tracks' clocks.
func TestTimescale_MultipleTracks(t *testing.T) {
	init := box("moov", trak(1, 57600), trak(2, 48000))

	_, err := fmp4.Timescale(init)
	assert.ErrorIs(t, err, fmp4.ErrAmbiguousTrack)
}

func TestTimescaleForTrack(t *testing.T) {
	init := box("moov", trak(1, 57600), trak(2, 48000))

	tests := map[string]struct {
		trackID  uint32
		expected uint32
		wantErr  error
	}{
		"first track":  {trackID: 1, expected: 57600},
		"second track": {trackID: 2, expected: 48000},
		"absent track": {trackID: 9, wantErr: fmp4.ErrNotFound},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := fmp4.TimescaleForTrack(init, tt.trackID)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}

// sample_count arrives off the wire, so a run that claims more samples than it
// could hold is rejected rather than multiplied into a duration that would
// become an EXTINF and a place on the ledger's timeline.
func TestFragmentDuration_ImplausibleSampleCount(t *testing.T) {
	t.Run("more entries than the trun holds", func(t *testing.T) {
		// flags 0x0301: data-offset, sample-duration and sample-size present, so
		// each entry is 8 bytes — but only one entry's worth follows.
		trun := box("trun", []byte{0, 0, 0x03, 0x01}, u32(1000), u32(0), u32(100), u32(500))
		fragment := box("moof", box("traf", box("tfhd", []byte{0, 0, 0, 0}, u32(1)), trun))

		_, err := fmp4.FragmentDuration(fragment)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "1000 samples")
	})

	t.Run("more samples than the media data could hold", func(t *testing.T) {
		// No per-sample entries at all, so only the mdat contradicts the count:
		// a sample occupies at least one byte.
		tfhd := box("tfhd", []byte{0, 0, 0, 0x08}, u32(1), u32(1920))
		trun := box("trun", []byte{0, 0, 0, 0x01}, u32(1_000_000), u32(0))
		fragment := append(
			box("moof", box("traf", tfhd, trun)),
			box("mdat", make([]byte, 4096))...,
		)

		_, err := fmp4.FragmentDuration(fragment)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bytes of media data")
	})

	t.Run("a plausible count is accepted", func(t *testing.T) {
		tfhd := box("tfhd", []byte{0, 0, 0, 0x08}, u32(1), u32(1920))
		trun := box("trun", []byte{0, 0, 0, 0x01}, u32(30), u32(0))
		fragment := append(
			box("moof", box("traf", tfhd, trun)),
			box("mdat", make([]byte, 4096))...,
		)

		got, err := fmp4.FragmentDuration(fragment)
		require.NoError(t, err)
		assert.Equal(t, uint64(30*1920), got)
	})
}

// A box may declare a size of 0, meaning it runs to the end of its parent. The
// scan has to honor that rather than treat it as a malformed length, or a
// perfectly valid init reads as having no boxes at all.
func TestTimescale_BoxRunsToEndOfParent(t *testing.T) {
	moov := box("moov", box("trak", box("mdia", mdhdV0(48000))))
	binary.BigEndian.PutUint32(moov[0:4], 0) // "to the end of the enclosing box"

	got, err := fmp4.Timescale(moov)
	require.NoError(t, err)
	assert.Equal(t, uint32(48000), got)
}

// The tfhd's default-sample-duration sits after whichever optional fields the
// flags announce, so the offset is computed rather than fixed. Getting that walk
// wrong reads an adjacent field as the duration — a plausible-looking number
// that would scale the whole manifest.
func TestFragmentDuration_TfhdOptionalFieldsShiftTheDefault(t *testing.T) {
	const samples, perSample = 25, 3600

	// flags 0x0b: base-data-offset (8 bytes), sample-description-index (4), and
	// default-sample-duration all present.
	tfhd := box("tfhd",
		[]byte{0, 0, 0, 0x0b},
		u32(1),    // track_ID
		u64(4096), // base_data_offset
		u32(1),    // sample_description_index
		u32(perSample),
	)
	trun := box("trun", []byte{0, 0, 0, 0x01}, u32(samples), u32(0))
	fragment := box("moof", box("traf", tfhd, trun))

	got, err := fmp4.FragmentDuration(fragment)
	require.NoError(t, err)
	assert.Equal(t, uint64(samples*perSample), got,
		"the default is read past base_data_offset and sample_description_index")
}

// first-sample-flags sits between the trun header and its entries, so the first
// per-sample duration is 4 bytes further in than it would otherwise be.
func TestFragmentDuration_TrunFirstSampleFlagsShiftTheEntries(t *testing.T) {
	durations := []uint32{900, 1000, 1100}
	var entries []byte
	for _, d := range durations {
		entries = append(entries, u32(d)...)
	}

	// flags 0x105: data-offset, first-sample-flags, and sample-duration present.
	trun := box("trun",
		[]byte{0, 0, 0x01, 0x05},
		u32(uint32(len(durations))),
		u32(0),          // data_offset
		u32(0x02000000), // first_sample_flags
		entries,
	)
	fragment := box("moof", box("traf", box("tfhd", []byte{0, 0, 0, 0}, u32(1)), trun))

	got, err := fmp4.FragmentDuration(fragment)
	require.NoError(t, err)
	assert.Equal(t, uint64(900+1000+1100), got)
}

// A fixed-framerate encoder states the duration once in the tfhd and omits it
// per sample, so the run's extent is the sample count times that default.
func TestFragmentDuration_TfhdDefault(t *testing.T) {
	const samples, perSample = 30, 1920

	fragment := box("moof",
		box("traf",
			// flags 0x000008: default-sample-duration present.
			box("tfhd", []byte{0, 0, 0, 0x08}, u32(1), u32(perSample)),
			// flags 0x000001: data-offset present, no per-sample durations.
			box("trun", []byte{0, 0, 0, 0x01}, u32(samples), u32(0)),
		),
	)

	got, err := fmp4.FragmentDuration(fragment)
	require.NoError(t, err)
	assert.Equal(t, uint64(samples*perSample), got)
}

// A variable-framerate run carries a duration per sample, and the extent is
// their sum — not the count times the first one.
func TestFragmentDuration_PerSample(t *testing.T) {
	durations := []uint32{1000, 1500, 900, 1100}
	var entries []byte
	for _, d := range durations {
		// sample_duration followed by sample_size, per the flags below.
		entries = append(entries, u32(d)...)
		entries = append(entries, u32(500)...)
	}

	fragment := box("moof",
		box("traf",
			box("tfhd", []byte{0, 0, 0, 0}, u32(1)),
			// flags 0x000301: data-offset, sample-duration, sample-size present.
			box("trun", []byte{0, 0, 0x03, 0x01}, u32(uint32(len(durations))), u32(0), entries),
		),
	)

	got, err := fmp4.FragmentDuration(fragment)
	require.NoError(t, err)
	assert.Equal(t, uint64(1000+1500+900+1100), got)
}

// Per-sample durations win over a tfhd default that also happens to be present.
func TestFragmentDuration_PerSampleOverridesDefault(t *testing.T) {
	fragment := box("moof",
		box("traf",
			box("tfhd", []byte{0, 0, 0, 0x08}, u32(1), u32(9999)),
			box("trun", []byte{0, 0, 0x01, 0x01}, u32(2), u32(0), u32(100), u32(200)),
		),
	)

	got, err := fmp4.FragmentDuration(fragment)
	require.NoError(t, err)
	assert.Equal(t, uint64(300), got)
}

// With neither per-sample durations nor a tfhd default there is nothing to
// measure, and inventing a value would be indistinguishable downstream from a
// real one.
func TestFragmentDuration_NoDurationAvailable(t *testing.T) {
	fragment := box("moof",
		box("traf",
			box("tfhd", []byte{0, 0, 0, 0}, u32(1)),
			box("trun", []byte{0, 0, 0, 0x01}, u32(30), u32(0)),
		),
	)

	_, err := fmp4.FragmentDuration(fragment)
	assert.ErrorIs(t, err, fmp4.ErrNotFound)
}

func TestFragmentDuration_MissingBoxes(t *testing.T) {
	tests := map[string]struct {
		fragment []byte
	}{
		"no moof": {fragment: box("mdat", []byte("payload"))},
		"no traf": {fragment: box("moof", box("mfhd", u32(1)))},
		"no trun": {fragment: box("moof", box("traf", box("tfhd", []byte{0, 0, 0, 0}, u32(1))))},
		"empty":   {fragment: nil},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := fmp4.FragmentDuration(tt.fragment)
			assert.ErrorIs(t, err, fmp4.ErrNotFound)
		})
	}
}

// A box scan must not run past its buffer on a size field that cannot be right,
// which is how malformed input turns into a panic rather than an error.
func TestFragmentDuration_MalformedSize(t *testing.T) {
	fragment := box("moof", box("traf"))
	// Overwrite the moof size with one that overruns the buffer.
	binary.BigEndian.PutUint32(fragment[0:4], 1<<30)

	_, err := fmp4.FragmentDuration(fragment)
	assert.ErrorIs(t, err, fmp4.ErrNotFound)
}

// A trun whose flags declare header fields it is too short to hold has no room
// for entries at all. The space left for them must not be computed as a negative
// length, which would wrap and wave any sample_count through to be multiplied
// into a duration.
func TestFragmentDuration_TrunShorterThanItsHeader(t *testing.T) {
	tests := map[string]struct {
		trun []byte
	}{
		// data-offset + sample-size present: the header alone needs 12 bytes.
		"missing data offset": {trun: box("trun", []byte{0, 0, 0x02, 0x01}, u32(0xFFFFFFFF))},
		// data-offset + first-sample-flags + sample-size: the header needs 16.
		"missing first sample flags": {trun: box("trun", []byte{0, 0, 0x02, 0x05}, u32(0xFFFFFFFF), u32(0))},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			tfhd := box("tfhd", []byte{0, 0, 0, 0x08}, u32(1), u32(1920))
			fragment := box("moof", box("traf", tfhd, tt.trun))

			duration, err := fmp4.FragmentDuration(fragment)
			require.Error(t, err)
			assert.Zero(t, duration)
		})
	}
}

// trakV1Tkhd is a trak whose tkhd is version 1: creation and modification times
// widen to 64 bits, moving track_ID eight bytes further in. Encoders emit this
// layout for timestamps that do not fit 32 bits.
func trakV1Tkhd(trackID, timescale uint32) []byte {
	tkhd := box("tkhd",
		[]byte{1, 0, 0, 0}, // version 1, flags
		u64(0), u64(0),     // creation, modification
		u32(trackID),
	)
	return box("trak", tkhd, box("mdia", mdhdV0(timescale)))
}

// The track_ID sits at a version-dependent offset, and the two tkhd layouts mix
// freely in one moov — an encoder may write v1 for one track and v0 for another.
func TestTimescaleForTrack_Version1Tkhd(t *testing.T) {
	init := box("moov", trak(1, 57600), trakV1Tkhd(2, 48000))

	ts, err := fmp4.TimescaleForTrack(init, 2)
	require.NoError(t, err)
	assert.Equal(t, uint32(48000), ts, "the ID is read from behind the 64-bit times")

	ts, err = fmp4.TimescaleForTrack(init, 1)
	require.NoError(t, err)
	assert.Equal(t, uint32(57600), ts, "the v0 sibling still resolves")
}

// --- Track identity edge cases ----------------------------------------------

// A v1 tkhd whose track_ID does not match must be skipped so the walk continues
// to the next trak. The negative match is as important as the positive one:
// getting the offset wrong would read garbage as the ID and either falsely match
// or skip a genuine match.
func TestTimescaleForTrack_Version1Tkhd_NonMatch(t *testing.T) {
	// Two v1-tkhd tracks, IDs 1 and 2.
	init := box("moov", trakV1Tkhd(1, 57600), trakV1Tkhd(2, 48000))

	ts, err := fmp4.TimescaleForTrack(init, 2)
	require.NoError(t, err)
	assert.Equal(t, uint32(48000), ts)

	_, err = fmp4.TimescaleForTrack(init, 3)
	assert.ErrorIs(t, err, fmp4.ErrNotFound,
		"a track_ID present in neither v1 trak must not match")
}

// An mdhd whose version is neither 0 nor 1 is unrecognized, and reading it must
// fail outright rather than interpret an unknown layout. It is not ErrNotFound:
// the box is there, so "absent" would send a caller looking elsewhere for it.
func TestTimescale_MdhdVersion2(t *testing.T) {
	mdhd := box("mdhd",
		[]byte{2, 0, 0, 0}, // version 2, flags
		u64(0), u64(0),     // creation, modification (64-bit for v1+)
		u32(48000),
		u64(0),
	)
	init := box("moov", box("trak", box("mdia", mdhd)))

	_, err := fmp4.Timescale(init)
	require.Error(t, err)
	assert.ErrorContains(t, err, "unsupported mdhd version 2")
	assert.NotErrorIs(t, err, fmp4.ErrNotFound)
}

// A trak with an mdia but no tkhd has no identity. TimescaleForTrack must skip
// it rather than treat the missing ID as zero.
func TestTimescaleForTrack_NoTkhd(t *testing.T) {
	// A trak without a tkhd — only mdia with a known timescale.
	init := box("moov",
		box("trak", box("mdia", mdhdV0(57600))),
		trak(1, 48000),
	)

	// Asking for track 1 must find the trak that HAS a tkhd.
	ts, err := fmp4.TimescaleForTrack(init, 1)
	require.NoError(t, err)
	assert.Equal(t, uint32(48000), ts)

	_, err = fmp4.TimescaleForTrack(init, 0)
	assert.ErrorIs(t, err, fmp4.ErrNotFound,
		"the trak without a tkhd must not match track_ID 0")
}

// --- Fuzz tests --------------------------------------------------------------

func FuzzTimescale(f *testing.F) {
	// Seed with every valid init from the table tests so the fuzzer starts
	// from known-good inputs and mutates from there.
	f.Add(initSegment(mdhdV0(57600)))
	f.Add(initSegment(mdhdV1(90000)))
	f.Add(box("moov", trak(1, 57600)))
	f.Add(box("moov", trak(1, 57600), trak(2, 48000)))
	f.Add(box("moov", trak(1, 57600), trakV1Tkhd(2, 48000)))
	// Edge cases that must not panic.
	f.Add([]byte{})
	f.Add(box("moov"))
	f.Add(box("moov", box("trak")))
	f.Add(box("moov", box("trak", box("mdia"))))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Neither call may panic, whatever the input.
		single, singleErr := fmp4.Timescale(data)
		one, oneErr := fmp4.TimescaleForTrack(data, 1)

		if singleErr != nil && single != 0 {
			t.Errorf("Timescale returned %d alongside error %v", single, singleErr)
		}
		if oneErr != nil && one != 0 {
			t.Errorf("TimescaleForTrack returned %d alongside error %v", one, oneErr)
		}
		// Timescale answers only for an init with a single track, so when both
		// succeed they are reading the same trak and must agree.
		if singleErr == nil && oneErr == nil && single != one {
			t.Errorf("Timescale = %d but TimescaleForTrack(1) = %d", single, one)
		}
	})
}

func FuzzFragmentDuration(f *testing.F) {
	// Seed with representative fragments.
	f.Add(box("moof",
		box("traf",
			box("tfhd", []byte{0, 0, 0, 0x08}, u32(1), u32(1920)),
			box("trun", []byte{0, 0, 0, 0x01}, u32(30), u32(0)),
		),
	))
	f.Add(box("moof",
		box("traf",
			box("tfhd", []byte{0, 0, 0, 0}, u32(1)),
			box("trun", []byte{0, 0, 0x03, 0x01}, u32(2), u32(0), u32(100), u32(500), u32(200), u32(500)),
		),
	))
	// With an mdat, which is what bounds a run that carries no entries.
	f.Add(append(
		box("moof", box("traf",
			box("tfhd", []byte{0, 0, 0, 0x08}, u32(1), u32(1920)),
			box("trun", []byte{0, 0, 0, 0x01}, u32(4), u32(0)),
		)),
		box("mdat", []byte("payload"))...,
	))
	// A trun shorter than the header its flags declare.
	f.Add(box("moof", box("traf",
		box("tfhd", []byte{0, 0, 0, 0x08}, u32(1), u32(1920)),
		box("trun", []byte{0, 0, 0x02, 0x01}, u32(0xFFFFFFFF)),
	)))
	// Edge cases.
	f.Add([]byte{})
	f.Add(box("moof"))
	f.Add(box("moof", box("traf")))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic, and a failure must not also hand back a duration.
		duration, err := fmp4.FragmentDuration(data)
		if err != nil && duration != 0 {
			t.Errorf("FragmentDuration returned %d alongside error %v", duration, err)
		}
	})
}

// --- Benchmarks --------------------------------------------------------------

func BenchmarkTimescale(b *testing.B) {
	init := initSegment(mdhdV0(90000))
	b.ReportAllocs()
	for b.Loop() {
		_, _ = fmp4.Timescale(init)
	}
}

func BenchmarkTimescaleForTrack(b *testing.B) {
	init := box("moov", trak(1, 57600), trakV1Tkhd(2, 48000))
	b.ReportAllocs()
	for b.Loop() {
		_, _ = fmp4.TimescaleForTrack(init, 2)
	}
}

func BenchmarkFragmentDuration(b *testing.B) {
	tfhd := box("tfhd", []byte{0, 0, 0, 0x08}, u32(1), u32(1920))
	trun := box("trun", []byte{0, 0, 0, 0x01}, u32(30), u32(0))
	fragment := box("moof", box("traf", tfhd, trun))
	b.ReportAllocs()
	for b.Loop() {
		_, _ = fmp4.FragmentDuration(fragment)
	}
}
