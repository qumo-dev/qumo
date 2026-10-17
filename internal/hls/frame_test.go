package hls

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// collectFrames reads every frame of payload, failing the test on an error.
func collectFrames(tb testing.TB, payload []byte) []frame {
	tb.Helper()
	var got []frame
	for f, err := range allFrames(payload) {
		require.NoError(tb, err)
		got = append(got, f)
	}
	return got
}

// The bytes are the draft's FRAME messages: a zigzag delta from the previous
// timestamp (the first from 0), a QUIC varint length, the payload. gomoqt's
// encoder writes exactly these for the same three frames, which is what keeps
// a stored group byte-identical to the wire.
func TestEncodeFrames_Wire(t *testing.T) {
	tests := map[string]struct {
		frames []frame
		want   []byte
	}{
		"no frames": {want: []byte{}},
		"forward and back": {
			frames: []frame{{Timestamp: 0, Body: []byte("a")}, {Timestamp: 3000, Body: []byte("bc")}, {Timestamp: 1500}},
			want: []byte{
				0x00, 0x01, 'a', // delta 0
				0x57, 0x70, 0x02, 'b', 'c', // zigzag(3000) = 6000 = 0x1770
				0x4b, 0xb7, 0x00, // zigzag(-1500) = 2999 = 0x0bb7
			},
		},
		// What qumo's publishers send today: every frame at timestamp 0, the
		// media time inside the LOC payload.
		"untimed frames": {
			frames: []frame{{Body: []byte("x")}, {Body: []byte("y")}},
			want:   []byte{0x00, 0x01, 'x', 0x00, 0x01, 'y'},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := encodeFrames(tt.frames)

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEncodeFrames_TimestampRange(t *testing.T) {
	tests := map[string][]frame{
		"past an int64":         {{Timestamp: math.MaxInt64 + 1}},
		"a delta past a varint": {{Timestamp: 0}, {Timestamp: math.MaxInt64}},
	}
	for name, frames := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := encodeFrames(frames)

			assert.ErrorIs(t, err, errTimestampRange)
		})
	}
}

func TestAllFrames_RoundTrip(t *testing.T) {
	frames := []frame{
		{Timestamp: 90000, Body: []byte("idr")},
		{Timestamp: 96000, Body: []byte("p")},
		{Timestamp: 93000, Body: []byte("b")},
		{Timestamp: 1<<61 - 1, Body: make([]byte, 70000)},
	}
	payload, err := encodeFrames(frames)
	require.NoError(t, err)

	assert.Equal(t, frames, collectFrames(t, payload))
}

func TestAllFrames_Malformed(t *testing.T) {
	tests := map[string]struct {
		payload []byte
		wantN   int // frames read before the error
	}{
		"a truncated delta":            {payload: []byte{0x40}},
		"a truncated length":           {payload: []byte{0x00}},
		"a body past the payload":      {payload: []byte{0x00, 0x02, 'a'}},
		"a frame, then a broken one":   {payload: []byte{0x00, 0x01, 'a', 0x00, 0x05}, wantN: 1},
		"a first timestamp below zero": {payload: []byte{0x01, 0x00}}, // zigzag 1 = -1
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			n := 0
			var last error
			for _, err := range allFrames(tt.payload) {
				if err != nil {
					last = err
					break
				}
				n++
			}

			assert.ErrorIs(t, last, errMalformed)
			assert.Equal(t, tt.wantN, n)
		})
	}
}

func TestAllFrames_StopsWhenAsked(t *testing.T) {
	payload, err := encodeFrames([]frame{{Timestamp: 1}, {Timestamp: 2}, {Timestamp: 3}})
	require.NoError(t, err)

	n := 0
	for range allFrames(payload) {
		n++
		break
	}

	assert.Equal(t, 1, n)
}

func TestVarint_Boundaries(t *testing.T) {
	for _, v := range []uint64{0, 63, 64, 16383, 16384, 1<<30 - 1, 1 << 30, maxVarint} {
		b := appendVarint(nil, v)

		got, n := readVarint(b)

		assert.Equal(t, v, got)
		assert.Equal(t, len(b), n)
	}
}
