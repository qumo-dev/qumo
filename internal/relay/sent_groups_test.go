package relay

import (
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
)

func TestSentGroups_ResetBefore(t *testing.T) {
	// Each case records streams at positions, then resets before earliest.
	tests := map[string]struct {
		positions []ringPosition
		earliest  ringPosition
		wantReset []ringPosition
		wantKept  []ringPosition
	}{
		"empty list resets nothing": {
			positions: nil, earliest: 5,
			wantReset: nil, wantKept: nil,
		},
		"all still in the ring": {
			positions: []ringPosition{5, 6, 7}, earliest: 5,
			wantReset: nil, wantKept: []ringPosition{5, 6, 7},
		},
		"oldest evicted": {
			positions: []ringPosition{4, 5, 6}, earliest: 5,
			wantReset: []ringPosition{4}, wantKept: []ringPosition{5, 6},
		},
		"all evicted": {
			positions: []ringPosition{1, 2, 3}, earliest: 9,
			wantReset: []ringPosition{1, 2, 3}, wantKept: nil,
		},
		"gap from a skip": {
			positions: []ringPosition{2, 3, 10, 11}, earliest: 4,
			wantReset: []ringPosition{2, 3}, wantKept: []ringPosition{10, 11},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var s sentGroups
			fakes := map[ringPosition]*fakeGroupCanceler{}
			for _, pos := range tt.positions {
				fakes[pos] = &fakeGroupCanceler{}
				s.add(pos, fakes[pos])
			}

			n := s.resetBefore(tt.earliest)

			assert.Equal(t, len(tt.wantReset), n)
			for _, pos := range tt.wantReset {
				assert.Equal(t, []moqt.GroupErrorCode{moqt.ExpiredGroupErrorCode}, fakes[pos].codes, "position %d is reset once, as expired", pos)
			}
			var kept []ringPosition
			for _, g := range s.groups {
				kept = append(kept, g.pos)
				assert.Empty(t, fakes[g.pos].codes, "position %d is still in the ring and not reset", g.pos)
			}
			assert.Equal(t, tt.wantKept, kept)
		})
	}
}

func TestSentGroups_ResetBefore_ClearsTail(t *testing.T) {
	// The compacted array must not keep references to reset streams.
	var s sentGroups
	for pos := ringPosition(1); pos <= 4; pos++ {
		s.add(pos, &fakeGroupCanceler{})
	}

	s.resetBefore(3)

	assert.Len(t, s.groups, 2)
	tail := s.groups[len(s.groups):cap(s.groups)]
	for i, g := range tail {
		assert.Nil(t, g.gw, "tail slot %d still references a stream", i)
	}
}

func TestSentGroups_ResetBefore_Twice(t *testing.T) {
	// A stream is reset once: a second call with the same bound finds nothing.
	var s sentGroups
	f := &fakeGroupCanceler{}
	s.add(1, f)
	s.add(2, &fakeGroupCanceler{})

	assert.Equal(t, 1, s.resetBefore(2))
	assert.Equal(t, 0, s.resetBefore(2))
	assert.Len(t, f.codes, 1)
}
