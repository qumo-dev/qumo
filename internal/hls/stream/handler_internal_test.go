package stream

import (
	"testing"
	"time"

	"github.com/okdaichi/qumo-ledger/ledger"
	"github.com/stretchr/testify/assert"
)

func TestParseSegmentID(t *testing.T) {
	tests := map[string]struct {
		base     string
		ext      string
		expected ledger.GroupID
		ok       bool
	}{
		"padded":             {base: "e000001-g00000042.m4s", ext: ".m4s", expected: ledger.NewGroupID(1, 42), ok: true},
		"unpadded":           {base: "e1-g42.m4s", ext: ".m4s", expected: ledger.NewGroupID(1, 42), ok: true},
		"other extension":    {base: "e000002-g00000000.ts", ext: ".ts", expected: ledger.NewGroupID(2, 0), ok: true},
		"wrong extension":    {base: "e000001-g00000042.ts", ext: ".m4s", ok: false},
		"missing extension":  {base: "e000001-g00000042", ext: ".m4s", expected: ledger.NewGroupID(1, 42), ok: true},
		"malformed":          {base: "segment.m4s", ext: ".m4s", ok: false},
		"non-numeric fields": {base: "ex-gy.m4s", ext: ".m4s", ok: false},
		// The zero GroupID names no group, so neither spelling of it is a segment.
		"zero id":        {base: "e000000-g00000000.m4s", ext: ".m4s", ok: false},
		"extension only": {base: ".m4s", ext: ".m4s", ok: false},
		"empty":          {base: "", ext: ".m4s", ok: false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			id, ok := parseSegmentID(tt.base, tt.ext)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.expected, id)
		})
	}
}

func TestMediaOffset(t *testing.T) {
	tests := map[string]struct {
		mediaTime int64
		timescale uint32
		expected  time.Duration
	}{
		"whole seconds": {mediaTime: 180000, timescale: 90000, expected: 2 * time.Second},
		"fraction":      {mediaTime: 45000, timescale: 90000, expected: 500 * time.Millisecond},
		"zero":          {mediaTime: 0, timescale: 90000, expected: 0},
		// A schema without a timescale cannot place media time at all; the
		// offset is zero rather than a division by zero.
		"zero timescale": {mediaTime: 180000, timescale: 0, expected: 0},
		// Far past where mediaTime × 1e9 would overflow an int64.
		"long presentation": {mediaTime: 90000 * 86400 * 365, timescale: 90000, expected: 365 * 24 * time.Hour},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.expected, mediaOffset(tt.mediaTime, tt.timescale))
		})
	}
}

func TestMaxSegmentSeconds(t *testing.T) {
	schema := ledger.TrackSchema{Timescale: 90000}
	tests := map[string]struct {
		durations []int64
		expected  int
	}{
		"none":             {durations: nil, expected: 0},
		"whole seconds":    {durations: []int64{180000, 360000, 180000}, expected: 4},
		"rounds up":        {durations: []int64{180001}, expected: 3},
		"sub-second":       {durations: []int64{1}, expected: 1},
		"ignores unset":    {durations: []int64{0, 180000, 0}, expected: 2},
		"ignores negative": {durations: []int64{-180000, 90000}, expected: 1},
		"nothing with one": {durations: []int64{0, 0}, expected: 0},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var groups []ledger.GroupInfo
			for i, duration := range tt.durations {
				groups = append(groups, ledger.GroupInfo{ID: ledger.NewGroupID(1, uint64(i)), Duration: duration})
			}

			assert.Equal(t, tt.expected, maxSegmentSeconds(schema, groups))
		})
	}
}

func TestDefaultSegmentExt(t *testing.T) {
	tests := map[string]struct {
		encoding string
		expected string
	}{
		"fmp4":    {encoding: "fmp4", expected: ".m4s"},
		"ts":      {encoding: "ts", expected: ".ts"},
		"mpegts":  {encoding: "mpegts", expected: ".ts"},
		"unknown": {encoding: "webm", expected: ".bin"},
		"unset":   {encoding: "", expected: ".bin"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.expected, defaultSegmentExt(ledger.TrackSchema{Encoding: tt.encoding}))
		})
	}
}
