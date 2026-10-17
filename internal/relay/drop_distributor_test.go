package relay

import (
	"sync"
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDrop returns a drop whose StartGroup and EndGroup are both seq, so a
// test can tell recorded drops apart by sequence.
func testDrop(seq moqt.GroupSequence) moqt.SubscribeDrop {
	return moqt.SubscribeDrop{StartGroup: seq, EndGroup: seq, ErrorCode: moqt.SubscribeErrorCodeInternal}
}

func TestNewDropDistributor(t *testing.T) {
	tests := map[string]struct {
		size     int
		wantSize int
	}{
		"positive size is kept":      {size: 3, wantSize: 3},
		"size one is kept":           {size: 1, wantSize: 1},
		"zero falls back to default": {size: 0, wantSize: dropRingSize},
		"negative falls back":        {size: -1, wantSize: dropRingSize},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			d := newDropDistributor(tt.size)
			require.NotNil(t, d)
			assert.Len(t, d.drops, tt.wantSize)
			assert.Equal(t, uint64(0), d.head())
		})
	}
}

func TestDropDistributor_Oldest(t *testing.T) {
	tests := map[string]struct {
		recorded int
		want     uint64
	}{
		"empty":          {recorded: 0, want: 0},
		"partly filled":  {recorded: 2, want: 0},
		"exactly full":   {recorded: 3, want: 0},
		"wrapped once":   {recorded: 4, want: 1},
		"wrapped deeply": {recorded: 10, want: 7},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			d := newDropDistributor(3)
			for i := range tt.recorded {
				seq := moqt.GroupSequence(i + 1)
				d.record(moqt.SubscribeDrop{StartGroup: seq, EndGroup: seq})
			}

			got := d.oldest()

			assert.Equal(t, tt.want, got)
			_, _, lost := d.since(got, nil)
			assert.Zero(t, lost, "reading from oldest loses nothing")
		})
	}
}

func TestDropDistributor_Record(t *testing.T) {
	d := newDropDistributor(2)

	for seq := moqt.GroupSequence(1); seq <= 3; seq++ {
		d.record(testDrop(seq))
		assert.Equal(t, uint64(seq), d.head())
	}

	// The third drop overwrote the first; the newest two remain.
	got, _, _ := d.since(0, nil)
	assert.Equal(t, []moqt.SubscribeDrop{testDrop(2), testDrop(3)}, got)
}

func TestDropDistributor_Since(t *testing.T) {
	tests := map[string]struct {
		size     int
		recorded int
		lastPos  uint64
		buf      []moqt.SubscribeDrop
		want     []moqt.SubscribeDrop
		wantPos  uint64
		wantLost uint64
	}{
		"empty distributor": {
			size: 4, recorded: 0, lastPos: 0,
			want: nil, wantPos: 0,
		},
		"all drops from the start": {
			size: 4, recorded: 3, lastPos: 0,
			want: []moqt.SubscribeDrop{testDrop(1), testDrop(2), testDrop(3)}, wantPos: 3,
		},
		"only drops after lastPos": {
			size: 4, recorded: 3, lastPos: 1,
			want: []moqt.SubscribeDrop{testDrop(2), testDrop(3)}, wantPos: 3,
		},
		"caught up returns nothing": {
			size: 4, recorded: 3, lastPos: 3,
			want: nil, wantPos: 3,
		},
		"lastPos ahead of head is kept": {
			size: 4, recorded: 2, lastPos: 5,
			want: nil, wantPos: 5,
		},
		"buffer exactly full loses nothing": {
			size: 3, recorded: 3, lastPos: 0,
			want: []moqt.SubscribeDrop{testDrop(1), testDrop(2), testDrop(3)}, wantPos: 3,
		},
		"overwritten drops are counted as lost": {
			size: 3, recorded: 5, lastPos: 0,
			want: []moqt.SubscribeDrop{testDrop(3), testDrop(4), testDrop(5)}, wantPos: 5, wantLost: 2,
		},
		"reader one behind the overwrite loses one": {
			size: 3, recorded: 5, lastPos: 1,
			want: []moqt.SubscribeDrop{testDrop(3), testDrop(4), testDrop(5)}, wantPos: 5, wantLost: 1,
		},
		"reader at the oldest kept drop loses nothing": {
			size: 3, recorded: 5, lastPos: 2,
			want: []moqt.SubscribeDrop{testDrop(3), testDrop(4), testDrop(5)}, wantPos: 5,
		},
		"appends to the given buffer": {
			size: 4, recorded: 1, lastPos: 0,
			buf:  []moqt.SubscribeDrop{testDrop(9)},
			want: []moqt.SubscribeDrop{testDrop(9), testDrop(1)}, wantPos: 1,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			d := newDropDistributor(tt.size)
			for seq := 1; seq <= tt.recorded; seq++ {
				d.record(testDrop(moqt.GroupSequence(seq)))
			}

			got, pos, lost := d.since(tt.lastPos, tt.buf)

			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantPos, pos)
			assert.Equal(t, tt.wantLost, lost)
		})
	}
}

// A reader running alongside a writer always sees a consistent window: the
// drops it gets are the newest ones up to the position it is given, and
// together with the lost count they account for every drop since its last
// position. Run with -race.
func TestDropDistributor_Since_ConcurrentRecord(t *testing.T) {
	const (
		size  = 4
		total = 5000
	)
	d := newDropDistributor(size)

	var wg sync.WaitGroup
	wg.Go(func() {
		for seq := 1; seq <= total; seq++ {
			d.record(testDrop(moqt.GroupSequence(seq)))
		}
	})

	var (
		pos uint64
		buf []moqt.SubscribeDrop
	)
	for pos < total {
		var (
			next uint64
			lost uint64
		)
		buf, next, lost = d.since(pos, buf[:0])
		if next == pos {
			continue
		}
		require.Equal(t, next-pos, uint64(len(buf))+lost, "drops plus lost must cover every position since the last read")
		for i, drop := range buf {
			want := moqt.GroupSequence(next - uint64(len(buf)) + uint64(i) + 1)
			require.Equal(t, want, drop.StartGroup, "drop at index %d of read ending at %d", i, next)
		}
		pos = next
	}
	wg.Wait()
}
