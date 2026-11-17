package abr

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/qumo-dev/gomoqt/msf"

	"github.com/qumo-dev/qumo/internal/ingest"
)

// H.264 NAL unit types (ITU-T H.264 Table 7-1).
const (
	nalSPS = 7
	nalPPS = 8
	nalAUD = 9
)

// A sourceFormat is how a source video track's frames are coded, and so what
// it takes to hand them to ffmpeg.
type sourceFormat int

const (
	// formatAVCC is H.264 with length-prefixed NAL units and its parameter
	// sets in the catalog, as RTMP and RTSP ingest publish it.
	formatAVCC sourceFormat = iota
	// formatAnnexB is H.264 with start codes and its parameter sets in the
	// stream, before each keyframe, as WebCodecs encodes it in "annexb" format.
	formatAnnexB
	// formatVP9 is VP9, whose configuration the codec string and picture size
	// state.
	formatVP9
)

func (f sourceFormat) String() string {
	switch f {
	case formatAVCC:
		return "H.264 (AVCC)"
	case formatAnnexB:
		return "H.264 (Annex B)"
	case formatVP9:
		return "VP9"
	}
	return fmt.Sprintf("format %d", int(f))
}

// formatOf reports how the catalog's video track t, with init data init, is
// coded, and false when the transcoder cannot take it.
//
// An avc1 track with init data is AVCC; one without carries its parameter
// sets in the stream, so its frames are Annex B. A VP9 track needs the full
// codec string (profile, level, bit depth) and its picture size, which
// together are its configuration.
func formatOf(t msf.Track, init []byte) (sourceFormat, bool) {
	switch {
	case isAVC(t.Codec) && init != nil:
		return formatAVCC, true
	case isAVC(t.Codec):
		return formatAnnexB, true
	case strings.HasPrefix(t.Codec, "vp09.") && strings.Count(t.Codec, ".") >= 3 &&
		t.Width != nil && t.Height != nil && *t.Width > 0 && *t.Height > 0:
		return formatVP9, true
	}
	return 0, false
}

// A source adapts a source video track's frames to the encoder's input, and
// knows the configuration the encoder decodes them with: at once for AVCC and
// VP9, and for Annex B from the parameter sets of the first keyframe.
type source struct {
	format sourceFormat
	codec  string
	// config is the encoder's view of the source, nil until known.
	config *sourceConfig
}

// newSource returns the source for track t, with init data init, coded as
// format.
func newSource(t msf.Track, init []byte, format sourceFormat) (*source, error) {
	s := &source{format: format, codec: t.Codec}
	switch format {
	case formatAVCC:
		cfg, err := ingest.ParseAVCConfig(append([]byte{0x17, 0, 0, 0, 0}, init...))
		if err != nil {
			return nil, fmt.Errorf("abr: source decoder configuration: %w", err)
		}
		s.config = &sourceConfig{Codec: t.Codec, Width: cfg.Width, Height: cfg.Height, Description: init}
	case formatVP9:
		s.config = &sourceConfig{Codec: t.Codec, Width: int(*t.Width), Height: int(*t.Height)}
	}
	return s, nil
}

// frame returns data, a frame of the source, as the encoder takes it, and
// false for a frame the encoder cannot take yet. first reports that the frame
// opens a group, so is a keyframe.
//
// An Annex B frame is rewritten with 4-byte lengths, dropping access unit
// delimiters. Until the source's configuration is known, its frames are
// refused but for a keyframe carrying its parameter sets, which makes it known.
func (s *source) frame(data []byte, first bool) ([]byte, bool) {
	if s.format != formatAnnexB {
		return data, s.config != nil
	}
	if !hasStartCode(data) {
		// Not Annex B after all: the publisher sends AVCC without stating
		// its configuration, which cannot be decoded.
		return nil, false
	}
	nalus := splitAnnexB(data)
	if s.config == nil {
		if !first {
			return nil, false
		}
		record, err := avcRecord(nalus)
		if err != nil {
			return nil, false
		}
		cfg, err := ingest.ParseAVCConfig(append([]byte{0x17, 0, 0, 0, 0}, record...))
		if err != nil {
			return nil, false
		}
		s.config = &sourceConfig{Codec: s.codec, Width: cfg.Width, Height: cfg.Height, Description: record}
	}
	return avcc(nalus), true
}

// hasStartCode reports whether b opens with an Annex B start code.
func hasStartCode(b []byte) bool {
	return len(b) >= 3 && b[0] == 0 && b[1] == 0 && (b[2] == 1 || (len(b) >= 4 && b[2] == 0 && b[3] == 1))
}

// splitAnnexB returns the NAL units of an Annex B byte stream, without their
// start codes.
func splitAnnexB(b []byte) [][]byte {
	var nalus [][]byte
	start := -1 // start of the current NAL unit, past its start code
	for i := 0; i+2 < len(b); {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				nalus = appendNALU(nalus, b[start:i])
			}
			i += 3
			start = i
			continue
		}
		i++
	}
	if start >= 0 {
		nalus = appendNALU(nalus, b[start:])
	}
	return nalus
}

// appendNALU appends n to nalus without the zero bytes that trail it: the
// leading zero of a 4-byte start code, and trailing_zero_8bits.
func appendNALU(nalus [][]byte, n []byte) [][]byte {
	for len(n) > 0 && n[len(n)-1] == 0 {
		n = n[:len(n)-1]
	}
	if len(n) == 0 {
		return nalus
	}
	return append(nalus, n)
}

// avcc returns nalus as an AVCC frame, each prefixed by its 4-byte length,
// leaving out access unit delimiters, which a sample does not carry.
func avcc(nalus [][]byte) []byte {
	size := 0
	for _, n := range nalus {
		size += 4 + len(n)
	}
	out := make([]byte, 0, size)
	for _, n := range nalus {
		if n[0]&0x1f == nalAUD {
			continue
		}
		out = binary.BigEndian.AppendUint32(out, uint32(len(n)))
		out = append(out, n...)
	}
	return out
}

// avcRecord builds the AVCDecoderConfigurationRecord of the parameter sets
// among nalus, with 4-byte NAL unit lengths.
func avcRecord(nalus [][]byte) ([]byte, error) {
	var sps, pps [][]byte
	for _, n := range nalus {
		switch n[0] & 0x1f {
		case nalSPS:
			sps = append(sps, n)
		case nalPPS:
			pps = append(pps, n)
		}
	}
	if len(sps) == 0 || len(pps) == 0 || len(sps[0]) < 4 {
		return nil, errors.New("abr: keyframe carries no SPS and PPS")
	}
	return ingest.BuildAVCDecoderConfigurationRecord(&ingest.AVCConfig{
		ProfileIDC:    sps[0][1],
		ProfileCompat: sps[0][2],
		LevelIDC:      sps[0][3],
		NALULenSize:   4,
		SPS:           sps,
		PPS:           pps,
	})
}
