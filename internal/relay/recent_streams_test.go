package relay

import (
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
)

func TestRecentStreams_Add(t *testing.T) {
	// Each case writes n streams interval apart, starting at t0, and checks
	// which were reset (by the order they were written, from 0).
	t0 := time.Unix(1_000_000, 0)
	tests := map[string]struct {
		n         int
		interval  time.Duration
		wantReset []int
	}{
		"at the minimum nothing is reset, however old": {
			n: recentStreamsMin, interval: time.Hour, wantReset: nil,
		},
		"past the minimum but young nothing is reset": {
			// 20 ms apart: the oldest of 20 is 380 ms old when the last is written.
			n: 20, interval: 20 * time.Millisecond, wantReset: nil,
		},
		"past the minimum, the old ones are reset": {
			// 100 ms apart: 0-7 are 400 ms or older when the last is written,
			// but the minimum keeps the newest 8, so only 0-3 are reset.
			n: 12, interval: 100 * time.Millisecond, wantReset: []int{0, 1, 2, 3},
		},
		"past the maximum, young ones are reset too": {
			n: recentStreamsMax + 3, interval: time.Millisecond, wantReset: []int{0, 1, 2},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var r recentStreams
			fakes := make([]*fakeGroupCanceler, tt.n)
			resets := 0
			for i := range fakes {
				fakes[i] = &fakeGroupCanceler{}
				resets += r.add(fakes[i], t0.Add(time.Duration(i)*tt.interval))
			}

			assert.Equal(t, len(tt.wantReset), resets)
			reset := map[int]bool{}
			for _, i := range tt.wantReset {
				reset[i] = true
			}
			for i, f := range fakes {
				if reset[i] {
					assert.Equal(t, []moqt.GroupErrorCode{moqt.ExpiredGroupErrorCode}, f.codes, "stream %d is reset once, as expired", i)
				} else {
					assert.Empty(t, f.codes, "stream %d is kept", i)
				}
			}
			assert.Len(t, r.streams, tt.n-len(tt.wantReset))
		})
	}
}

func TestRecentStreams_Add_KeepsMinimumWhenIdle(t *testing.T) {
	// A publisher that pauses: the next write after a long gap resets only
	// down to the minimum, never below it.
	t0 := time.Unix(1_000_000, 0)
	var r recentStreams
	for i := range recentStreamsMin + 4 {
		r.add(&fakeGroupCanceler{}, t0.Add(time.Duration(i)*time.Millisecond))
	}

	n := r.add(&fakeGroupCanceler{}, t0.Add(time.Hour))

	assert.Equal(t, 5, n, "all but the minimum are now too old")
	assert.Len(t, r.streams, recentStreamsMin)
}

func TestRecentStreams_Add_ClearsTail(t *testing.T) {
	// The compacted array must not keep references to reset streams.
	t0 := time.Unix(1_000_000, 0)
	var r recentStreams
	for i := range recentStreamsMin + 3 {
		r.add(&fakeGroupCanceler{}, t0.Add(time.Duration(i)*time.Second))
	}

	tail := r.streams[len(r.streams):cap(r.streams)]
	for i, s := range tail {
		assert.Nil(t, s.gw, "tail slot %d still references a stream", i)
	}
}
