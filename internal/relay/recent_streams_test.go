package relay

import (
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
)

func TestRecentStreams_Add(t *testing.T) {
	// Each case adds n streams to a ring of size max, then checks which were
	// reset (by the order they were added, from 0) and how many adds reported a
	// reset.
	tests := map[string]struct {
		max       int
		n         int
		wantReset []int
	}{
		"below the limit resets nothing": {max: 3, n: 2, wantReset: nil},
		"at the limit resets nothing":    {max: 3, n: 3, wantReset: nil},
		"one past resets the oldest":     {max: 3, n: 4, wantReset: []int{0}},
		"wraps around in order":          {max: 3, n: 8, wantReset: []int{0, 1, 2, 3, 4}},
		"a ring of one keeps the newest": {max: 1, n: 3, wantReset: []int{0, 1}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := recentStreams{max: tt.max}
			fakes := make([]*fakeGroupCanceler, tt.n)
			resets := 0
			for i := range fakes {
				fakes[i] = &fakeGroupCanceler{}
				if r.add(fakes[i]) {
					resets++
				}
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
					assert.Empty(t, f.codes, "stream %d is among the newest and not reset", i)
				}
			}
		})
	}
}
