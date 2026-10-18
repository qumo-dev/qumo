// Package cmaf packages encoded media frames into fragmented MP4.
//
// It exists because the wire and the archive want different things. MoQ carries
// LOC — an encoded frame with a timestamp and almost nothing else — which is
// what makes sub-second playback possible and what a WebCodecs decoder consumes
// directly. HLS wants CMAF: an initialization segment describing the codec, and
// media fragments carrying samples with durations. Neither format is wrong; they
// serve different consumers.
//
// Packaging therefore happens here, at the subscriber, rather than at the
// publisher. A publisher that muxed CMAF would push its GOP batching onto every
// consumer including the real-time one, and would store a representation that no
// longer matches what it sent. Converting here keeps the live path unbatched and
// leaves one publisher feeding both.
package cmaf

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/mp4"
)

// Timescale is the unit fragment timestamps are expressed in.
//
// LOC timestamps come from WebCodecs, which counts microseconds, so a
// microsecond timescale carries them with no scaling and no rounding. A coarser
// one — 90 kHz, say — would round every sample and accumulate the error along
// the timeline.
const Timescale = 1_000_000

// trackID is the single video track every init segment describes. Audio is not
// packaged yet; when it is, it becomes track 2 rather than a second file.
const trackID = 1

// Frame is one encoded frame as it arrived over the wire: a presentation
// timestamp in [Timescale] units, and the encoded bytes.
//
// Sync is whether the frame opens a group. LOC carries no such flag — a MoQ
// group begins at each keyframe, so the group boundary is the signal, and the
// caller passes what it observed rather than this package guessing.
type Frame struct {
	Timestamp uint64
	Sync      bool
	Data      []byte
}

// Packager turns groups of frames into CMAF. One Packager serves one track for
// as long as its codec configuration holds; a publisher that restarts with a
// different configuration needs a new one, which is also a new ledger epoch.
// It holds only the init segment, so concurrent requests share it.
type Packager struct {
	init []byte
}

// VideoConfig is what a catalog states about a track, and all this package needs
// to describe it: the WebCodecs codec string, and the coded picture size.
type VideoConfig struct {
	// Codec is the WebCodecs identifier, e.g. "vp09.00.10.08" or "avc1.42E01E".
	Codec string

	Width  uint16
	Height uint16

	// Description is the codec's out-of-band configuration, when it has one:
	// the AVC/HEVC parameter sets a decoder needs before the first frame. VP9
	// and AV1 carry theirs in the codec string, so this is empty for them.
	Description []byte
}

// NewPackager builds the initialization segment for cfg, which fixes the codec
// for every fragment the packager goes on to produce.
func NewPackager(cfg VideoConfig) (*Packager, error) {
	if cfg.Width == 0 || cfg.Height == 0 {
		return nil, fmt.Errorf("cmaf: track has no picture size (%dx%d)", cfg.Width, cfg.Height)
	}

	init := mp4.CreateEmptyInit()
	init.AddEmptyTrack(Timescale, "video", "und")
	trak := init.Moov.Trak

	if err := describe(trak, cfg); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		return nil, fmt.Errorf("cmaf: encode init segment: %w", err)
	}
	return &Packager{init: buf.Bytes()}, nil
}

// Init is the initialization segment: ftyp + moov, the bytes an HLS
// EXT-X-MAP points at.
func (p *Packager) Init() []byte {
	return p.init
}

// Fragment packages one group of frames into a moof+mdat media fragment, the
// sequence-th of the track, placed at decodeTime on the track's timeline, and
// reports the media extent it covers.
//
// decodeTime is passed in rather than accumulated here because the caller is
// already keeping it: it is the same running total the ledger records as a
// group's media time, and the manifest publishes. Two counters for one quantity
// drift the moment they disagree about whether a group counted — an append the
// ledger refuses would advance one and not the other, and every later fragment
// would claim a position the manifest does not list it at.
//
// It deliberately is not the encoder's clock. A capture timestamp starts
// wherever the browser's happened to be, thousands of seconds in, while the
// manifest describes a track starting at zero; a fragment stamped with the
// capture clock lands nowhere near where a player seeks, so the buffer fills
// while nothing overlaps the seek position and playback stalls silently.
//
// Sample durations come from the gaps between timestamps, which leaves the last
// frame of a group without one — its successor belongs to the next group. The
// caller states it as last: the writer that stored the group decided it when it
// recorded the group's duration, so the fragment covers
// exactly the extent the manifest lists.
func (p *Packager) Fragment(sequence uint32, frames []Frame, decodeTime uint64, last uint32) (data []byte, duration uint64, err error) {
	if len(frames) == 0 {
		return nil, 0, fmt.Errorf("cmaf: no frames to package")
	}
	if last == 0 || last > maxSampleDuration {
		return nil, 0, fmt.Errorf("cmaf: last frame duration %d outside (0, %d]", last, uint64(maxSampleDuration))
	}

	gaps, err := frameGaps(frames)
	if err != nil {
		return nil, 0, err
	}
	durations := append(gaps, last)
	var total uint64
	for _, d := range durations {
		total += uint64(d)
	}

	frag, err := mp4.CreateFragment(sequence, trackID)
	if err != nil {
		return nil, 0, fmt.Errorf("cmaf: create fragment: %w", err)
	}

	for i, f := range frames {
		flags := mp4.NonSyncSampleFlags
		if f.Sync {
			flags = mp4.SyncSampleFlags
		}
		frag.AddFullSample(mp4.FullSample{
			Sample:     mp4.NewSample(flags, durations[i], uint32(len(f.Data)), 0),
			DecodeTime: decodeTime,
			Data:       f.Data,
		})
		decodeTime += uint64(durations[i])
	}

	var buf bytes.Buffer
	if err := frag.Encode(&buf); err != nil {
		return nil, 0, fmt.Errorf("cmaf: encode fragment: %w", err)
	}
	return buf.Bytes(), total, nil
}

// maxSampleDuration bounds a plausible gap between two frames. Timestamps arrive
// off the wire, and a gap beyond this is a corrupt one rather than slow media —
// worth saying so, because the difference is invisible once it has been
// truncated into a 32-bit sample duration and written into a trun.
const maxSampleDuration = 10 * Timescale

// frameGaps derives each frame's extent but the last's from the gap to the
// next one.
//
// Timestamps must advance: a gap of zero, one that runs backwards, or one
// beyond [maxSampleDuration] means the group cannot be measured, and a group
// that cannot be measured is skipped rather than given an invented extent. A
// backwards gap is the dangerous case — unsigned subtraction turns it into a
// huge number that truncates to noise, which reads downstream exactly like a
// duration somebody meant.
func frameGaps(frames []Frame) ([]uint32, error) {
	gaps := make([]uint32, 0, len(frames))
	for i := range len(frames) - 1 {
		this, next := frames[i].Timestamp, frames[i+1].Timestamp
		if next <= this {
			return nil, fmt.Errorf(
				"cmaf: frame %d timestamp %d does not advance on %d", i+1, next, this)
		}
		delta := next - this
		if delta > maxSampleDuration {
			return nil, fmt.Errorf(
				"cmaf: frame %d is %d units after its predecessor, beyond %d",
				i+1, delta, uint64(maxSampleDuration))
		}
		gaps = append(gaps, uint32(delta))
	}
	return gaps, nil
}

// describe attaches the codec-specific sample description to the track.
func describe(trak *mp4.TrakBox, cfg VideoConfig) error {
	switch family(cfg.Codec) {
	case "vp09", "vp08":
		vpcc, err := vpxConfig(cfg.Codec)
		if err != nil {
			return err
		}
		if err := trak.SetVPxDescriptor(family(cfg.Codec), vpcc, cfg.Width, cfg.Height); err != nil {
			return fmt.Errorf("cmaf: describe %s: %w", cfg.Codec, err)
		}
		return nil

	case "avc1", "avc3":
		sps, pps, err := avcParameterSets(cfg.Description)
		if err != nil {
			return err
		}
		if err := trak.SetAVCDescriptor("avc1", sps, pps, true); err != nil {
			return fmt.Errorf("cmaf: describe %s: %w", cfg.Codec, err)
		}
		return nil

	default:
		return fmt.Errorf("cmaf: unsupported codec %q", cfg.Codec)
	}
}

// family is the four-character prefix of a WebCodecs codec string.
func family(codec string) string {
	if len(codec) < 4 {
		return codec
	}
	return codec[:4]
}

// vpxConfig builds a vpcC from a WebCodecs VP8/VP9 codec string, which states
// profile, level and bit depth as dot-separated decimal fields:
//
//	vp09.<profile>.<level>.<bitDepth>[.<chroma>.<primaries>.<transfer>.<matrix>.<fullRange>]
//
// Only the first three are required. The colour fields default to BT.709, which
// is what a browser camera capture produces and what the encoder assumed when
// the publisher omitted them.
func vpxConfig(codec string) (*mp4.VppCBox, error) {
	parts := strings.Split(codec, ".")
	if len(parts) < 4 {
		return nil, fmt.Errorf("cmaf: codec %q states no profile, level and bit depth", codec)
	}

	field := func(i int, name string) (byte, error) {
		v, err := strconv.ParseUint(parts[i], 10, 8)
		if err != nil {
			return 0, fmt.Errorf("cmaf: codec %q has an unreadable %s: %w", codec, name, err)
		}
		return byte(v), nil
	}

	profile, err := field(1, "profile")
	if err != nil {
		return nil, err
	}
	level, err := field(2, "level")
	if err != nil {
		return nil, err
	}
	bitDepth, err := field(3, "bit depth")
	if err != nil {
		return nil, err
	}

	vpcc := &mp4.VppCBox{
		Version:  1,
		Profile:  profile,
		Level:    level,
		BitDepth: bitDepth,
		// 4:2:0 co-sited, BT.709 primaries/transfer/matrix, limited range.
		ChromaSubsampling:       1,
		ColourPrimaries:         1,
		TransferCharacteristics: 1,
		MatrixCoefficients:      1,
		VideoFullRangeFlag:      0,
	}
	for i, target := range []*byte{
		&vpcc.ChromaSubsampling, &vpcc.ColourPrimaries,
		&vpcc.TransferCharacteristics, &vpcc.MatrixCoefficients, &vpcc.VideoFullRangeFlag,
	} {
		if idx := 4 + i; idx < len(parts) {
			v, err := field(idx, "colour field")
			if err != nil {
				return nil, err
			}
			*target = v
		}
	}
	return vpcc, nil
}

// avcParameterSets splits an AVCDecoderConfigurationRecord — what WebCodecs
// hands over as decoderConfig.description — into the SPS and PPS an avcC needs.
func avcParameterSets(description []byte) (sps, pps [][]byte, err error) {
	if len(description) == 0 {
		return nil, nil, fmt.Errorf(
			"cmaf: an AVC track needs its parameter sets, and the catalog carries none")
	}
	record, err := avc.DecodeAVCDecConfRec(description)
	if err != nil {
		return nil, nil, fmt.Errorf("cmaf: read AVC configuration: %w", err)
	}
	return record.SPSnalus, record.PPSnalus, nil
}
