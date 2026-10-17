// Package fmp4 reads the few facts a ledger writer needs out of fragmented-MP4
// bytes: how long a fragment runs, and in what timescale.
//
// A writer must supply a group's Duration, and the manifest renderers require
// it — an HLS EXTINF and a DASH @d have nowhere else to come from. A
// fragment already carries its own duration, so an ingester that instead assumes
// one from configuration is stating something it did not measure: when the
// assumed value and the encoder's real GOP disagree, the manifest advertises the
// wrong extent and players drift or stall, with nothing in the pipeline
// contradicting it.
//
// This package is deliberately small and read-only. The ledger core stores
// opaque bytes and takes no interest in containers; this is the ingest-side
// helper that lets a fragmented-MP4 producer fill in a truthful GroupInfo, and
// it lives here rather than in one ingester because every fragmented-MP4
// producer needs exactly the same computation.
package fmp4

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrNotFound reports that a box the caller asked about is absent — an init
// segment with no mdhd, or a fragment with no trun. It is returned rather than
// guessed at: a duration invented here would be indistinguishable from a
// measured one downstream.
var ErrNotFound = errors.New("fmp4: box not found")

// boxHeaderSize is a box's 32-bit size followed by its 4-character type.
const boxHeaderSize = 8

// fullBoxFlagMask selects the three flag bytes of a FullBox header, dropping the
// version byte above them.
const fullBoxFlagMask = 0x00ffffff

// ErrAmbiguousTrack reports that an initialization segment describes more than
// one track, so there is no single timescale to return. Use [TimescaleForTrack]
// to name which one. Guessing here would scale every duration the caller
// computes by the ratio between two tracks' timescales, silently.
var ErrAmbiguousTrack = errors.New("fmp4: init describes more than one track")

// Timescale reads the media timescale from a single-track initialization
// segment — the number of units per second that a fragment's durations are
// expressed in.
//
// It is taken from the track's mdhd (moov > trak > mdia > mdhd) rather than the
// movie-wide mvhd, because sample durations are in the media timescale and the
// two commonly differ. An init describing several tracks returns
// [ErrAmbiguousTrack].
func Timescale(init []byte) (uint32, error) {
	moov, err := find(init, "moov")
	if err != nil {
		return 0, fmt.Errorf("fmp4: timescale: %w", err)
	}

	traks := children(moov, "trak")
	switch len(traks) {
	case 0:
		return 0, fmt.Errorf("fmp4: timescale: %w: trak", ErrNotFound)
	case 1:
		return trackTimescale(traks[0])
	default:
		return 0, fmt.Errorf("fmp4: timescale: %w (%d tracks)", ErrAmbiguousTrack, len(traks))
	}
}

// TimescaleForTrack reads the media timescale of the track with the given
// track_ID, for an initialization segment describing more than one.
func TimescaleForTrack(init []byte, trackID uint32) (uint32, error) {
	moov, err := find(init, "moov")
	if err != nil {
		return 0, fmt.Errorf("fmp4: timescale: %w", err)
	}

	for _, trak := range children(moov, "trak") {
		id, ok := trackHeaderID(trak)
		if !ok || id != trackID {
			continue
		}
		return trackTimescale(trak)
	}
	return 0, fmt.Errorf("fmp4: timescale: %w: track_ID %d", ErrNotFound, trackID)
}

// trackHeaderID reads a trak's track_ID from its tkhd. The field sits after the
// creation and modification times, which version 1 widens to 64 bits.
func trackHeaderID(trak []byte) (uint32, bool) {
	tkhd, ok := child(trak, "tkhd")
	if !ok || len(tkhd) < 4 {
		return 0, false
	}
	at := 12 // version(1) flags(3) creation(4) modification(4)
	if tkhd[0] == 1 {
		at = 20 // version(1) flags(3) creation(8) modification(8)
	}
	if at+4 > len(tkhd) {
		return 0, false
	}
	return binary.BigEndian.Uint32(tkhd[at : at+4]), true
}

// trackTimescale reads one trak's media timescale from its mdhd.
func trackTimescale(trak []byte) (uint32, error) {
	mdhd, err := find(trak, "mdia", "mdhd")
	if err != nil {
		return 0, fmt.Errorf("fmp4: timescale: %w", err)
	}
	if len(mdhd) < 4 {
		return 0, errors.New("fmp4: mdhd truncated")
	}

	// mdhd is a FullBox: one version byte, three flag bytes, then version-
	// dependent fields. Version 1 widens creation/modification time and
	// duration to 64 bits, which moves timescale.
	switch version := mdhd[0]; version {
	case 0:
		// version(1) flags(3) creation(4) modification(4) timescale(4)
		if len(mdhd) < 20 {
			return 0, errors.New("fmp4: mdhd v0 truncated")
		}
		return binary.BigEndian.Uint32(mdhd[12:16]), nil
	case 1:
		// version(1) flags(3) creation(8) modification(8) timescale(4)
		if len(mdhd) < 28 {
			return 0, errors.New("fmp4: mdhd v1 truncated")
		}
		return binary.BigEndian.Uint32(mdhd[20:24]), nil
	default:
		return 0, fmt.Errorf("fmp4: unsupported mdhd version %d", version)
	}
}

// FragmentDuration sums a fragment's sample durations, in the media timescale
// returned by [Timescale].
//
// Durations come from the fragment's trun, falling back per-sample to the tfhd's
// default-sample-duration — the common case, since a fixed-framerate encoder
// states the duration once in the tfhd rather than repeating it per sample.
func FragmentDuration(fragment []byte) (uint64, error) {
	moof, err := find(fragment, "moof")
	if err != nil {
		return 0, fmt.Errorf("fmp4: fragment duration: %w", err)
	}
	traf, err := find(moof, "traf")
	if err != nil {
		return 0, fmt.Errorf("fmp4: fragment duration: %w", err)
	}

	// The tfhd default applies to every sample whose trun omits a duration.
	var defaultDuration uint32
	if tfhd, err := find(traf, "tfhd"); err == nil {
		defaultDuration = tfhdDefaultSampleDuration(tfhd)
	}

	trun, err := find(traf, "trun")
	if err != nil {
		return 0, fmt.Errorf("fmp4: fragment duration: %w", err)
	}

	// A sample occupies at least one byte of media data, so the mdat bounds how
	// many samples the run can legitimately declare. Without it a run that
	// carries no per-sample entries — nothing whose length contradicts the
	// count — could claim any sample_count it liked and have it multiplied into
	// a duration that becomes an EXTINF and a place on the ledger's timeline.
	var maxSamples uint64
	if mdat, ok := child(fragment, "mdat"); ok {
		maxSamples = uint64(len(mdat))
	}

	return trunDuration(trun, defaultDuration, maxSamples)
}

// tfhdDefaultSampleDuration returns the tfhd's default-sample-duration, or zero
// when the box does not carry one. The field is optional and its offset depends
// on which earlier optional fields are present, so the flags drive the walk.
func tfhdDefaultSampleDuration(tfhd []byte) uint32 {
	if len(tfhd) < 8 {
		return 0
	}
	// A FullBox opens with a version byte and three flag bytes; masking the
	// version off the leading word is the same read.
	flags := binary.BigEndian.Uint32(tfhd[:4]) & fullBoxFlagMask

	const (
		baseDataOffsetPresent         = 0x000001
		sampleDescriptionIndexPresent = 0x000002
		defaultSampleDurationPresent  = 0x000008
	)
	if flags&defaultSampleDurationPresent == 0 {
		return 0
	}

	at := 8 // version(1) flags(3) track_ID(4)
	if flags&baseDataOffsetPresent != 0 {
		at += 8
	}
	if flags&sampleDescriptionIndexPresent != 0 {
		at += 4
	}
	if at+4 > len(tfhd) {
		return 0
	}
	return binary.BigEndian.Uint32(tfhd[at : at+4])
}

// trunDuration sums the run's sample durations, using defaultDuration for
// samples whose duration the run omits. maxSamples bounds a plausible
// sample_count, or is zero when nothing bounds it.
func trunDuration(trun []byte, defaultDuration uint32, maxSamples uint64) (uint64, error) {
	if len(trun) < 8 {
		return 0, errors.New("fmp4: trun truncated")
	}
	flags := binary.BigEndian.Uint32(trun[:4]) & fullBoxFlagMask
	count := uint64(binary.BigEndian.Uint32(trun[4:8]))

	const (
		dataOffsetPresent        = 0x000001
		firstSampleFlagsPresent  = 0x000004
		sampleDurationPresent    = 0x000100
		sampleSizePresent        = 0x000200
		sampleFlagsPresent       = 0x000400
		sampleCompositionPresent = 0x000800
	)

	at := 8 // version(1) flags(3) sample_count(4)
	if flags&dataOffsetPresent != 0 {
		at += 4
	}
	if flags&firstSampleFlagsPresent != 0 {
		at += 4
	}
	// The flags are read off the wire too: a run shorter than the header they
	// declare has no entries, and the space left for them must not go negative.
	if at > len(trun) {
		return 0, errors.New("fmp4: trun truncated")
	}

	// Every optional per-sample field contributes 4 bytes to each entry, so the
	// entries are what the declared count can be checked against.
	var stride uint64
	for _, present := range []uint32{
		sampleDurationPresent, sampleSizePresent, sampleFlagsPresent, sampleCompositionPresent,
	} {
		if flags&present != 0 {
			stride += 4
		}
	}

	// The count is read off the wire, so it is checked before it is multiplied
	// into a duration or used to walk the box. Entries give the stronger bound;
	// with none, the media data is all that constrains it.
	switch {
	case stride > 0:
		if available := uint64(len(trun) - at); count*stride > available {
			return 0, fmt.Errorf(
				"fmp4: trun declares %d samples but holds %d bytes of entries", count, available)
		}
	case maxSamples > 0 && count > maxSamples:
		return 0, fmt.Errorf(
			"fmp4: trun declares %d samples for %d bytes of media data", count, maxSamples)
	}

	// Without per-sample durations the whole run is count × the tfhd default.
	if flags&sampleDurationPresent == 0 {
		if defaultDuration == 0 {
			return 0, fmt.Errorf(
				"fmp4: trun has no sample durations and tfhd has no default: %w", ErrNotFound)
		}
		return count * uint64(defaultDuration), nil
	}

	// Sample duration leads each entry; the remaining fields only affect stride.
	var total uint64
	for range count {
		if at+4 > len(trun) {
			return 0, fmt.Errorf("fmp4: trun truncated at sample offset %d", at)
		}
		total += uint64(binary.BigEndian.Uint32(trun[at : at+4]))
		at += int(stride)
	}
	return total, nil
}

// find walks a box path, returning the payload of the last element. Each name
// is searched among the children of the previous one, so find(b, "moov",
// "trak", "mdia", "mdhd") descends that nesting.
func find(data []byte, path ...string) ([]byte, error) {
	current := data
	for _, name := range path {
		payload, ok := child(current, name)
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		current = payload
	}
	return current, nil
}

// children scans one level of boxes and returns every matching payload, for the
// containers that legitimately repeat — a moov holds one trak per track.
func children(data []byte, name string) [][]byte {
	var out [][]byte
	eachBox(data, func(typ string, payload []byte) {
		if typ == name {
			out = append(out, payload)
		}
	})
	return out
}

// child scans one level of boxes for name, returning its payload.
func child(data []byte, name string) ([]byte, bool) {
	var found []byte
	var ok bool
	eachBox(data, func(typ string, payload []byte) {
		if !ok && typ == name {
			found, ok = payload, true
		}
	})
	return found, ok
}

// eachBox walks one level of boxes, calling visit with each one's type and
// payload. A malformed length ends the walk rather than resynchronizing on a
// guess: past that point the offsets mean nothing.
func eachBox(data []byte, visit func(typ string, payload []byte)) {
	for at := 0; at+boxHeaderSize <= len(data); {
		size := int(binary.BigEndian.Uint32(data[at : at+4]))
		typ := string(data[at+4 : at+8])

		// A size of 0 means the box runs to the end of its parent, which is a
		// length this scan can honor. A size of 1 means the real length is a
		// 64-bit largesize after the header — only seen on a very large mdat,
		// which is a sibling of the boxes read here rather than one of them —
		// and reading the 1 as a length would desynchronize the scan, so the
		// scan stops instead. Any size that overruns the buffer is malformed.
		switch {
		case size == 0:
			size = len(data) - at
		case size < boxHeaderSize || at+size > len(data):
			return
		}

		visit(typ, data[at+boxHeaderSize:at+size])
		at += size
	}
}
