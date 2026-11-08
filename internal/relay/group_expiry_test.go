package relay

import (
	"math"
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// step is one egress action on a groupExpiry: open the next group's stream,
// or write a frame with a timestamp on the latest one.
type step struct {
	open bool
	ts   uint64
}

func TestGroupExpiry_Frame(t *testing.T) {
	// Groups are numbered by the order they were opened, from 0.
	tests := map[string]struct {
		maxAge    uint64
		steps     []step
		wantReset []int
	}{
		"max age 0 keeps only the latest once it presents a frame": {
			maxAge:    0,
			steps:     []step{{open: true}, {ts: 0}, {open: true}, {ts: 20}},
			wantReset: []int{0},
		},
		"a group is not expired before the next presents a frame": {
			maxAge:    0,
			steps:     []step{{open: true}, {ts: 0}, {open: true}},
			wantReset: nil,
		},
		"one unit short of max age keeps the group": {
			// Group 0's reach is group 1's first frame (20); 119-20 = 99.
			maxAge:    100,
			steps:     []step{{open: true}, {ts: 0}, {open: true}, {ts: 20}, {ts: 119}},
			wantReset: nil,
		},
		"an age equal to max age expires the group": {
			maxAge:    100,
			steps:     []step{{open: true}, {ts: 0}, {open: true}, {ts: 20}, {ts: 120}},
			wantReset: []int{0},
		},
		"the latest group never expires": {
			maxAge:    100,
			steps:     []step{{open: true}, {ts: 0}, {ts: 1_000_000}},
			wantReset: nil,
		},
		"expires oldest first, up to the first that is young enough": {
			// Reaches: group 0 -> 20, group 1 -> 40, group 2 -> 60; latest 140.
			maxAge: 100,
			steps: []step{
				{open: true}, {ts: 0}, {open: true}, {ts: 20}, {open: true}, {ts: 40},
				{open: true}, {ts: 60}, {ts: 140},
			},
			wantReset: []int{0, 1},
		},
		"timestamps that run backwards are age 0": {
			maxAge:    1,
			steps:     []step{{open: true}, {ts: 500}, {open: true}, {ts: 100}},
			wantReset: nil,
		},
		"frames without timestamps expire only under max age 0": {
			maxAge:    100,
			steps:     []step{{open: true}, {ts: 0}, {open: true}, {ts: 0}, {open: true}, {ts: 0}},
			wantReset: nil,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var e groupExpiry
			var fakes []*fakeGroupCanceler
			resets := 0
			for _, s := range tt.steps {
				if s.open {
					f := &fakeGroupCanceler{}
					fakes = append(fakes, f)
					resets += e.open(f)
					continue
				}
				resets += e.frame(s.ts, tt.maxAge)
			}

			assert.Equal(t, len(tt.wantReset), resets)
			reset := map[int]bool{}
			for _, i := range tt.wantReset {
				reset[i] = true
			}
			for i, f := range fakes {
				if reset[i] {
					assert.Equal(t, []moqt.GroupErrorCode{moqt.ExpiredGroupErrorCode}, f.codes, "group %d is reset once, as expired", i)
				} else {
					assert.Empty(t, f.codes, "group %d is kept", i)
				}
			}
		})
	}
}

func TestGroupExpiry_Frame_KeepsTheRing(t *testing.T) {
	// With keep 2, a group is reset only once 2 newer groups were opened,
	// however expired it is (max age 0).
	e := groupExpiry{keep: 2}
	fakes := make([]*fakeGroupCanceler, 4)
	resets := 0
	for i := range fakes {
		fakes[i] = &fakeGroupCanceler{}
		resets += e.open(fakes[i])
		resets += e.frame(uint64(i*20), 0)
		if i < 2 {
			assert.Zero(t, resets, "after group %d, every group is still in the ring", i)
		}
	}

	assert.Equal(t, 2, resets)
	assert.Len(t, fakes[0].codes, 1, "group 0 left the ring when group 2 opened")
	assert.Len(t, fakes[1].codes, 1, "group 1 left the ring when group 3 opened")
	assert.Empty(t, fakes[2].codes)
	assert.Empty(t, fakes[3].codes)
	assert.Len(t, e.streams, 2)
}

func TestGroupExpiry_Open_CapsStreams(t *testing.T) {
	// With no frames to measure age by, only the cap bounds the streams kept.
	var e groupExpiry
	fakes := make([]*fakeGroupCanceler, maxLiveGroupStreams+3)
	resets := 0
	for i := range fakes {
		fakes[i] = &fakeGroupCanceler{}
		resets += e.open(fakes[i])
	}

	assert.Equal(t, 3, resets)
	for i, f := range fakes {
		if i < 3 {
			assert.Len(t, f.codes, 1, "group %d is past the cap and reset", i)
		} else {
			assert.Empty(t, f.codes, "group %d is kept", i)
		}
	}
	require.Len(t, e.streams, maxLiveGroupStreams)
	for i, s := range e.streams[len(e.streams):cap(e.streams)] {
		assert.Nil(t, s.gw, "tail slot %d still references a reset stream", i)
	}
}

func TestMaxAgeUnits(t *testing.T) {
	tests := map[string]struct {
		ms, timescale, want uint64
	}{
		"default timescale is milliseconds": {ms: 400, timescale: 0, want: 400},
		"millisecond timescale":             {ms: 400, timescale: 1000, want: 400},
		"90 kHz video":                      {ms: 400, timescale: 90_000, want: 36_000},
		"48 kHz audio":                      {ms: 20, timescale: 48_000, want: 960},
		"zero stays zero":                   {ms: 0, timescale: 90_000, want: 0},
		"saturates instead of overflowing":  {ms: math.MaxUint64, timescale: 90_000, want: math.MaxUint64},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, maxAgeUnits(tt.ms, tt.timescale))
		})
	}
}

func TestSubscriberMaxLatency(t *testing.T) {
	tests := map[string]struct {
		requested, want uint64
	}{
		"0 is unset and gets the relay's": {requested: 0, want: relayMaxLatency},
		"a shorter one is kept":           {requested: 100, want: 100},
		"the relay's exactly":             {requested: relayMaxLatency, want: relayMaxLatency},
		"a longer one is capped":          {requested: 60_000, want: relayMaxLatency},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, subscriberMaxLatency(tt.requested))
		})
	}
}
