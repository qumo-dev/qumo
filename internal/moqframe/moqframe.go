// Package moqframe writes and reads a group's frames in the moq-lite-05
// encoding: the FRAME messages of draft-lcurley-moq-lite-05 (section 7.18),
// concatenated exactly as they follow the GROUP header on a group stream.
//
//	FRAME {
//	  Timestamp Delta (i)  zigzag-mapped, from the previous frame; the first from 0
//	  Message Length (i)
//	  Payload (b)
//	}
//
// (i) is a QUIC variable-length integer (RFC 9000, section 16). It is how the
// HLS egress stores a group in the ledger (qumo-ledger decision 15): byte for
// byte what MoQ carried, built from each moqt.Frame's Timestamp and Body.
// gomoqt encodes the same layout on the wire but does not export it, so the
// golden bytes in the tests are taken from the draft and pin the two together.
package moqframe

import (
	"errors"
	"fmt"
	"iter"
	"math"
)

// Encoding is the ledger TrackSchema.Encoding of a track stored this way.
const Encoding = "moq-lite-05"

var (
	// ErrMalformed reports a payload that is not a run of whole FRAME
	// messages: a truncated integer or payload, or a timestamp outside the
	// timeline.
	ErrMalformed = errors.New("moqframe: malformed payload")

	// ErrTimestampRange reports a timestamp, or a delta between two, that the
	// encoding cannot carry: timestamps fit an int64, and a zigzag-mapped
	// delta a QUIC varint (62 bits).
	ErrTimestampRange = errors.New("moqframe: timestamp out of range")
)

// Frame is one frame of a group.
type Frame struct {
	// Timestamp is the frame's moq-lite timestamp, as the publisher set it.
	Timestamp uint64
	// Body is the frame's payload. A Frame read by [All] aliases the payload
	// it came from.
	Body []byte
}

// maxVarint is the largest value a QUIC varint carries.
const maxVarint = 1<<62 - 1

// Encode returns frames as a group payload, in the order given.
func Encode(frames []Frame) ([]byte, error) {
	size := 0
	for _, f := range frames {
		size += 16 + len(f.Body)
	}
	payload := make([]byte, 0, size)
	var prev uint64
	for i, f := range frames {
		if f.Timestamp > math.MaxInt64 {
			return nil, fmt.Errorf("frame %d: %w: %d", i, ErrTimestampRange, f.Timestamp)
		}
		delta := zigzag(int64(f.Timestamp) - int64(prev))
		if delta > maxVarint {
			return nil, fmt.Errorf("frame %d: %w: delta from %d to %d", i, ErrTimestampRange, prev, f.Timestamp)
		}
		payload = appendVarint(payload, delta)
		payload = appendVarint(payload, uint64(len(f.Body)))
		payload = append(payload, f.Body...)
		prev = f.Timestamp
	}
	return payload, nil
}

// All iterates the frames of a group payload in order. A malformed payload
// yields the frames before the fault, then one error wrapping [ErrMalformed].
func All(payload []byte) iter.Seq2[Frame, error] {
	return func(yield func(Frame, error) bool) {
		var prev uint64
		for i := 0; len(payload) > 0; i++ {
			delta, n := readVarint(payload)
			if n == 0 {
				yield(Frame{}, fmt.Errorf("frame %d: %w: truncated timestamp delta", i, ErrMalformed))
				return
			}
			payload = payload[n:]
			length, n := readVarint(payload)
			if n == 0 {
				yield(Frame{}, fmt.Errorf("frame %d: %w: truncated length", i, ErrMalformed))
				return
			}
			payload = payload[n:]
			if length > uint64(len(payload)) {
				yield(Frame{}, fmt.Errorf("frame %d: %w: length %d past the payload's %d bytes", i, ErrMalformed, length, len(payload)))
				return
			}
			ts, ok := addDelta(prev, unzigzag(delta))
			if !ok {
				yield(Frame{}, fmt.Errorf("frame %d: %w: timestamp outside the timeline", i, ErrMalformed))
				return
			}
			body := payload[:length:length]
			payload = payload[length:]
			if !yield(Frame{Timestamp: ts, Body: body}, nil) {
				return
			}
			prev = ts
		}
	}
}

// zigzag maps a signed delta onto an unsigned one, small magnitudes either
// side of zero to small values, as the draft specifies.
func zigzag(s int64) uint64 { return uint64(s<<1) ^ uint64(s>>63) }

// unzigzag inverts [zigzag].
func unzigzag(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

// addDelta returns prev+delta, and false when the result leaves the timeline:
// below zero or past an int64.
func addDelta(prev uint64, delta int64) (uint64, bool) {
	// prev never exceeds MaxInt64: it is zero or a timestamp addDelta made.
	sum := int64(prev) + delta
	if delta > 0 && sum < int64(prev) {
		return 0, false
	}
	if sum < 0 {
		return 0, false
	}
	return uint64(sum), true
}

// appendVarint appends v, which must not exceed maxVarint, as a QUIC
// variable-length integer in its shortest form.
func appendVarint(b []byte, v uint64) []byte {
	switch {
	case v < 1<<6:
		return append(b, byte(v))
	case v < 1<<14:
		return append(b, 0x40|byte(v>>8), byte(v))
	case v < 1<<30:
		return append(b, 0x80|byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	default:
		return append(b, 0xc0|byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32),
			byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	}
}

// readVarint reads a QUIC variable-length integer from the front of b and
// returns it with the bytes it took, or n == 0 when b is too short.
func readVarint(b []byte) (v uint64, n int) {
	if len(b) == 0 {
		return 0, 0
	}
	n = 1 << (b[0] >> 6)
	if len(b) < n {
		return 0, 0
	}
	v = uint64(b[0] & 0x3f)
	for _, c := range b[1:n] {
		v = v<<8 | uint64(c)
	}
	return v, n
}
