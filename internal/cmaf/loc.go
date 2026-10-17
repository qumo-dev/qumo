package cmaf

import (
	"encoding/binary"
	"fmt"
)

// DecodeLOC reads one LOC frame: a QUIC variable-length timestamp in
// microseconds, a variable-length payload size, then the encoded frame.
//
// This is the whole container. A frame carries no duration, no codec
// configuration and no keyframe flag — that is what makes it low overhead, and
// what leaves the packager to recover durations from timestamps and sync points
// from group boundaries. Everything a decoder needs beyond the bytes lives in
// the catalog instead, stated once.
func DecodeLOC(b []byte) (timestamp uint64, payload []byte, err error) {
	timestamp, n, err := varint(b)
	if err != nil {
		return 0, nil, fmt.Errorf("cmaf: loc timestamp: %w", err)
	}
	b = b[n:]

	size, n, err := varint(b)
	if err != nil {
		return 0, nil, fmt.Errorf("cmaf: loc payload size: %w", err)
	}
	b = b[n:]

	if uint64(len(b)) < size {
		return 0, nil, fmt.Errorf("cmaf: loc frame declares %d bytes but holds %d", size, len(b))
	}
	return timestamp, b[:size], nil
}

// EncodeLOC is the inverse of [DecodeLOC]: it writes a LOC frame — a QUIC varint
// timestamp in microseconds, a QUIC varint payload length, then the payload —
// into a fresh buffer. Publishers (the dev seeder, tests) build frames with this;
// the egress reads them back with [DecodeLOC].
func EncodeLOC(timestamp uint64, payload []byte) []byte {
	var b []byte
	b = appendVarint(b, timestamp)
	b = appendVarint(b, uint64(len(payload)))
	return append(b, payload...)
}

// varint decodes a QUIC variable-length integer (RFC 9000 §16): the top two bits
// of the first byte give the encoding's length, and the remaining bits are the
// value's most significant.
func varint(b []byte) (value uint64, size int, err error) {
	if len(b) == 0 {
		return 0, 0, fmt.Errorf("no bytes")
	}

	size = 1 << (b[0] >> 6)
	if len(b) < size {
		return 0, 0, fmt.Errorf("need %d bytes, have %d", size, len(b))
	}

	// The length prefix is not part of the value.
	switch size {
	case 1:
		return uint64(b[0] & 0x3f), 1, nil
	case 2:
		return uint64(binary.BigEndian.Uint16(b[:2]) & 0x3fff), 2, nil
	case 4:
		return uint64(binary.BigEndian.Uint32(b[:4]) & 0x3fff_ffff), 4, nil
	default:
		return binary.BigEndian.Uint64(b[:8]) & 0x3fff_ffff_ffff_ffff, 8, nil
	}
}

// appendVarint encodes v as a QUIC variable-length integer (RFC 9000 §16): the
// inverse of [varint]. The top two bits of the first byte carry the encoding
// length (1, 2, 4, or 8 bytes); the remaining bits are the value's most
// significant bits.
func appendVarint(b []byte, v uint64) []byte {
	switch {
	case v < 1<<6:
		return append(b, byte(v))
	case v < 1<<14:
		return append(b, byte(v>>8)|0x40, byte(v))
	case v < 1<<30:
		return append(b, byte(v>>24)|0x80, byte(v>>16), byte(v>>8), byte(v))
	default:
		return append(b,
			byte(v>>56)|0xc0, byte(v>>48), byte(v>>40), byte(v>>32),
			byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	}
}

// FramesFromLOC decodes a group's frames, each body one LOC frame, into the
// frames [Packager.Fragment] packages, in order. The first is the sync sample:
// a MoQ group begins at a keyframe, and LOC states no flag of its own. Each
// Data aliases the body it came from.
func FramesFromLOC(bodies [][]byte) ([]Frame, error) {
	frames := make([]Frame, 0, len(bodies))
	for i, body := range bodies {
		timestamp, payload, err := DecodeLOC(body)
		if err != nil {
			return nil, fmt.Errorf("frame %d: %w", i, err)
		}
		frames = append(frames, Frame{Timestamp: timestamp, Sync: i == 0, Data: payload})
	}
	return frames, nil
}
