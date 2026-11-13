package relay

import (
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// opening is one groupExpiry.open: a group that arrived at arrival, opened at
// now (nanoseconds on monoNanos's clock).
type opening struct {
	arrival, now int64
}

func TestGroupExpiry_Open(t *testing.T) {
	// Groups are numbered by the order they were opened, from 0. keep is 0, so
	// the ring's window does not apply.
	tests := map[string]struct {
		maxLatency time.Duration
		openings   []opening
		wantReset  []int
	}{
		"a group expires once the next arrived max latency ago": {
			maxLatency: 100,
			openings:   []opening{{0, 0}, {20, 20}, {120, 120}},
			wantReset:  []int{0},
		},
		"one nanosecond short keeps the group": {
			maxLatency: 100,
			openings:   []opening{{0, 0}, {20, 20}, {119, 119}},
			wantReset:  nil,
		},
		"age is measured from the next group's arrival, not the group's own": {
			// Group 0 arrived long ago, but group 1 only 60 before now.
			maxLatency: 100,
			openings:   []opening{{0, 0}, {90, 90}, {150, 150}},
			wantReset:  nil,
		},
		"the latest group never expires": {
			maxLatency: 100,
			openings:   []opening{{0, 1_000_000}},
			wantReset:  nil,
		},
		"expires oldest first, up to the first that is young enough": {
			// Group 3 opened late (egress behind): groups 1 and 2 arrived 120
			// and 100 before it, group 3 itself 80.
			maxLatency: 100,
			openings:   []opening{{0, 0}, {20, 20}, {40, 40}, {60, 140}},
			wantReset:  []int{0, 1},
		},
		"max latency 0 keeps only the latest": {
			maxLatency: 0,
			openings:   []opening{{0, 0}, {20, 20}, {40, 40}},
			wantReset:  []int{0, 1},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var e groupExpiry
			fakes := make([]*fakeGroupCanceler, len(tt.openings))
			resets := 0
			for i, o := range tt.openings {
				fakes[i] = &fakeGroupCanceler{}
				resets += e.open(fakes[i], o.arrival, o.now, tt.maxLatency)
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

func TestGroupExpiry_Open_KeepsTheRing(t *testing.T) {
	// With keep 2, a group is reset only once 2 newer groups were opened,
	// however expired it is (max latency 0).
	e := groupExpiry{keep: 2}
	fakes := make([]*fakeGroupCanceler, 4)
	resets := 0
	for i := range fakes {
		fakes[i] = &fakeGroupCanceler{}
		at := int64(i * 20)
		resets += e.open(fakes[i], at, at, 0)
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
	// Groups that all arrived at once never expire by age; only the cap bounds
	// the streams kept.
	var e groupExpiry
	fakes := make([]*fakeGroupCanceler, maxLiveGroupStreams+3)
	resets := 0
	for i := range fakes {
		fakes[i] = &fakeGroupCanceler{}
		resets += e.open(fakes[i], 0, 0, relayMaxLatency)
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

func TestSubscriberMaxLatency(t *testing.T) {
	tests := map[string]struct {
		requestedMs uint64
		want        time.Duration
	}{
		"0 is unset and gets the relay's": {requestedMs: 0, want: relayMaxLatency},
		"a shorter one is kept":           {requestedMs: 100, want: 100 * time.Millisecond},
		"the relay's exactly":             {requestedMs: 400, want: relayMaxLatency},
		"a longer one is capped":          {requestedMs: 60_000, want: relayMaxLatency},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, subscriberMaxLatency(tt.requestedMs))
		})
	}
}
