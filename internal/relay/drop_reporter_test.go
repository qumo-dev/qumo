package relay

import (
	"errors"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
)

func dropRange(start, end moqt.GroupSequence, code moqt.SubscribeErrorCode) moqt.SubscribeDrop {
	return moqt.SubscribeDrop{StartGroup: start, EndGroup: end, ErrorCode: code}
}

const (
	codeInternal = moqt.SubscribeErrorCodeInternal
	codeTimeout  = moqt.SubscribeErrorCodeTimeout
)

func TestDropReporter_Report(t *testing.T) {
	tests := map[string]struct {
		sent  []moqt.GroupSequence
		prior []moqt.SubscribeDrop
		drop  moqt.SubscribeDrop
		want  []moqt.SubscribeDrop
	}{
		"nothing is sent before the first group": {
			sent: nil,
			drop: dropRange(3, 4, codeInternal),
			want: nil,
		},
		"range after start is sent unchanged": {
			sent: []moqt.GroupSequence{2},
			drop: dropRange(3, 4, codeTimeout),
			want: []moqt.SubscribeDrop{dropRange(3, 4, codeTimeout)},
		},
		"single group range": {
			sent: []moqt.GroupSequence{2},
			drop: dropRange(5, 5, codeTimeout),
			want: []moqt.SubscribeDrop{dropRange(5, 5, codeTimeout)},
		},
		"range ending before start is ignored": {
			sent: []moqt.GroupSequence{10},
			drop: dropRange(3, 9, codeInternal),
			want: nil,
		},
		"range across start is trimmed past the sent start": {
			sent: []moqt.GroupSequence{10},
			drop: dropRange(3, 12, codeInternal),
			want: []moqt.SubscribeDrop{dropRange(11, 12, codeInternal)},
		},
		"inverted range is ignored": {
			sent: []moqt.GroupSequence{1},
			drop: dropRange(5, 4, codeInternal),
			want: nil,
		},
		"sent groups are left out": {
			sent: []moqt.GroupSequence{1, 3, 5},
			drop: dropRange(1, 6, codeInternal),
			want: []moqt.SubscribeDrop{
				dropRange(2, 2, codeInternal),
				dropRange(4, 4, codeInternal),
				dropRange(6, 6, codeInternal),
			},
		},
		"range of sent groups only sends nothing": {
			sent: []moqt.GroupSequence{1, 2, 3},
			drop: dropRange(2, 3, codeInternal),
			want: nil,
		},
		"range already reported is not resent": {
			sent:  []moqt.GroupSequence{1},
			prior: []moqt.SubscribeDrop{dropRange(3, 9, codeInternal)},
			drop:  dropRange(4, 6, codeTimeout),
			want:  nil,
		},
		"overlap sends only the new parts with the new code": {
			sent:  []moqt.GroupSequence{1},
			prior: []moqt.SubscribeDrop{dropRange(5, 7, codeTimeout)},
			drop:  dropRange(3, 9, codeInternal),
			want:  []moqt.SubscribeDrop{dropRange(3, 4, codeInternal), dropRange(8, 9, codeInternal)},
		},
		"range spanning two earlier ranges sends the gaps": {
			sent:  []moqt.GroupSequence{1},
			prior: []moqt.SubscribeDrop{dropRange(3, 4, codeInternal), dropRange(7, 8, codeInternal)},
			drop:  dropRange(2, 10, codeTimeout),
			want: []moqt.SubscribeDrop{
				dropRange(2, 2, codeTimeout),
				dropRange(5, 6, codeTimeout),
				dropRange(9, 10, codeTimeout),
			},
		},
		"range touching an earlier one is sent": {
			sent:  []moqt.GroupSequence{1},
			prior: []moqt.SubscribeDrop{dropRange(5, 7, codeInternal)},
			drop:  dropRange(8, 9, codeInternal),
			want:  []moqt.SubscribeDrop{dropRange(8, 9, codeInternal)},
		},
		"range before an earlier one is sent": {
			sent:  []moqt.GroupSequence{1},
			prior: []moqt.SubscribeDrop{dropRange(5, 7, codeInternal)},
			drop:  dropRange(2, 3, codeInternal),
			want:  []moqt.SubscribeDrop{dropRange(2, 3, codeInternal)},
		},
		"group zero is never reported": {
			sent: []moqt.GroupSequence{0},
			drop: dropRange(0, 2, codeInternal),
			want: []moqt.SubscribeDrop{dropRange(1, 2, codeInternal)},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			w := &fakeDropWriter{}
			r := newDropReporter(w, newDropDistributor(4))
			for _, seq := range tt.sent {
				r.sent(seq)
			}
			for _, drop := range tt.prior {
				r.report(drop)
			}
			w.sent = nil

			r.report(tt.drop)

			assert.Equal(t, tt.want, w.sent)
		})
	}
}

func TestDropReporter_Report_WriteErrorStillCountsAsHandled(t *testing.T) {
	w := &fakeDropWriter{err: errors.New("stream closed")}
	r := newDropReporter(w, newDropDistributor(4))
	r.sent(1)

	r.report(dropRange(3, 4, codeInternal))
	r.report(dropRange(3, 4, codeInternal))

	// A failed write is not retried: the subscribe stream is gone, and the next
	// group delivery ends egress.
	assert.Equal(t, []moqt.SubscribeDrop{dropRange(3, 4, codeInternal)}, w.sent)
	assert.True(t, r.isHandled(3))
}

func TestDropReporter_ReportSkipped(t *testing.T) {
	tests := map[string]struct {
		sent    []moqt.GroupSequence
		prior   []moqt.SubscribeDrop
		next    moqt.GroupSequence
		highest moqt.GroupSequence
		want    []moqt.SubscribeDrop
	}{
		"nothing before the first group": {
			sent: nil,
			next: 6, highest: 6,
			want: nil,
		},
		"in-order skip reports the gap": {
			sent: []moqt.GroupSequence{1, 2},
			next: 6, highest: 6,
			want: []moqt.SubscribeDrop{dropRange(3, 5, codeInternal)},
		},
		"next group reports nothing": {
			sent: []moqt.GroupSequence{1, 2},
			next: 3, highest: 3,
			want: nil,
		},
		"next at start reports nothing": {
			sent: []moqt.GroupSequence{4},
			next: 4, highest: 4,
			want: nil,
		},
		"next below start reports nothing": {
			sent: []moqt.GroupSequence{4},
			next: 2, highest: 4,
			want: nil,
		},
		// Group 5 arrived after 6 and was evicted unsent: it is below the
		// highest sent group but must still be reported.
		"out-of-order groups below the highest sent are reported": {
			sent: []moqt.GroupSequence{1, 6, 2},
			next: 10, highest: 10,
			want: []moqt.SubscribeDrop{dropRange(3, 5, codeInternal), dropRange(7, 9, codeInternal)},
		},
		// Groups 6..8 arrived before 5 and were evicted: they are above the
		// group sent next but must still be reported.
		"evicted groups above next are reported": {
			sent: []moqt.GroupSequence{1},
			next: 5, highest: 8,
			want: []moqt.SubscribeDrop{dropRange(2, 4, codeInternal), dropRange(6, 8, codeInternal)},
		},
		"groups already reported are left out": {
			sent:  []moqt.GroupSequence{1},
			prior: []moqt.SubscribeDrop{dropRange(3, 4, codeTimeout)},
			next:  7, highest: 7,
			want: []moqt.SubscribeDrop{dropRange(2, 2, codeInternal), dropRange(5, 6, codeInternal)},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			w := &fakeDropWriter{}
			r := newDropReporter(w, newDropDistributor(4))
			for _, seq := range tt.sent {
				r.sent(seq)
			}
			for _, drop := range tt.prior {
				r.report(drop)
			}
			w.sent = nil

			r.reportSkipped(tt.next, tt.highest, codeInternal)

			assert.Equal(t, tt.want, w.sent)
			if !slices.Contains(tt.sent, tt.next) {
				assert.False(t, r.isHandled(tt.next), "the group sent next is never reported")
			}
		})
	}
}

func TestDropReporter_Sent_FirstSetsStart(t *testing.T) {
	w := &fakeDropWriter{}
	r := newDropReporter(w, newDropDistributor(4))

	r.sent(5)
	r.sent(2) // out of order, below the start
	r.report(dropRange(3, 7, codeInternal))

	assert.Equal(t, []moqt.SubscribeDrop{dropRange(6, 7, codeInternal)}, w.sent)
}

func TestDropReporter_IsHandled(t *testing.T) {
	r := newDropReporter(&fakeDropWriter{}, newDropDistributor(4))
	r.sent(1)
	r.report(dropRange(3, 5, codeInternal))
	r.report(dropRange(9, 9, codeTimeout))

	tests := map[string]struct {
		seq  moqt.GroupSequence
		want bool
	}{
		"below every span":     {seq: 0, want: false},
		"sent group":           {seq: 1, want: true},
		"between sent and gap": {seq: 2, want: false},
		"first in a span":      {seq: 3, want: true},
		"inside a span":        {seq: 4, want: true},
		"last in a span":       {seq: 5, want: true},
		"between spans":        {seq: 6, want: false},
		"single-group span":    {seq: 9, want: true},
		"above every span":     {seq: 10, want: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, r.isHandled(tt.seq))
		})
	}
}

func TestDropReporter_IsHandled_Empty(t *testing.T) {
	r := newDropReporter(&fakeDropWriter{}, newDropDistributor(4))

	assert.False(t, r.isHandled(1))
}

// Past the cap the two oldest spans are joined, so the gap between them counts
// as handled and the number of spans stays bounded.
func TestDropReporter_Add_JoinsOldestSpansPastCap(t *testing.T) {
	r := newDropReporter(&fakeDropWriter{}, newDropDistributor(4))
	r.sent(1)

	// Every other group from 3, so no two spans merge on their own.
	for i := range maxHandledSpans {
		seq := moqt.GroupSequence(3 + 2*i)
		r.report(dropRange(seq, seq, codeTimeout))
	}

	assert.Len(t, r.handled, maxHandledSpans)
	assert.True(t, r.isHandled(2), "the gap between the two oldest spans is joined")
	assert.False(t, r.isHandled(4))
	assert.True(t, r.isHandled(moqt.GroupSequence(3+2*(maxHandledSpans-1))))
}

func TestDropReporter_Flush(t *testing.T) {
	tests := map[string]struct {
		recorded []moqt.SubscribeDrop
		start    moqt.GroupSequence
		want     []moqt.SubscribeDrop
	}{
		"nothing recorded": {
			start: 1,
			want:  nil,
		},
		"forwards upstream drops with their codes": {
			recorded: []moqt.SubscribeDrop{dropRange(2, 4, codeInternal), dropRange(6, 8, codeTimeout)},
			start:    1,
			want:     []moqt.SubscribeDrop{dropRange(2, 4, codeInternal), dropRange(6, 8, codeTimeout)},
		},
		"drops before the first group are not replayed": {
			recorded: []moqt.SubscribeDrop{dropRange(2, 4, codeInternal), dropRange(6, 8, codeInternal)},
			start:    5,
			want:     []moqt.SubscribeDrop{dropRange(6, 8, codeInternal)},
		},
		"drop across the first group leaves it out": {
			recorded: []moqt.SubscribeDrop{dropRange(2, 8, codeInternal)},
			start:    5,
			want:     []moqt.SubscribeDrop{dropRange(6, 8, codeInternal)},
		},
		"repeated upstream drop is sent once": {
			recorded: []moqt.SubscribeDrop{dropRange(2, 4, codeInternal), dropRange(3, 4, codeInternal)},
			start:    1,
			want:     []moqt.SubscribeDrop{dropRange(2, 4, codeInternal)},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			src := newDropDistributor(4)
			for _, drop := range tt.recorded {
				src.record(drop)
			}
			w := &fakeDropWriter{}
			r := newDropReporter(w, src)
			r.sent(tt.start)

			r.flush()

			assert.Equal(t, tt.want, w.sent)
		})
	}
}

func TestDropReporter_Flush_WaitsForFirstGroup(t *testing.T) {
	src := newDropDistributor(4)
	src.record(dropRange(3, 4, codeInternal))
	w := &fakeDropWriter{}
	r := newDropReporter(w, src)

	r.flush()
	assert.Empty(t, w.sent, "nothing is sent before the first group")

	r.sent(1)
	r.flush()
	assert.Equal(t, []moqt.SubscribeDrop{dropRange(3, 4, codeInternal)}, w.sent)
}

func TestDropReporter_Flush_OnlyNewDrops(t *testing.T) {
	src := newDropDistributor(4)
	w := &fakeDropWriter{}
	r := newDropReporter(w, src)
	r.sent(1)

	src.record(dropRange(2, 2, codeInternal))
	r.flush()
	r.flush()
	src.record(dropRange(5, 6, codeInternal))
	r.flush()

	assert.Equal(t, []moqt.SubscribeDrop{dropRange(2, 2, codeInternal), dropRange(5, 6, codeInternal)}, w.sent)
}

func TestDropReporter_Flush_CountsLostDrops(t *testing.T) {
	src := newDropDistributor(2)
	for seq := moqt.GroupSequence(2); seq <= 6; seq++ {
		src.record(dropRange(seq, seq, codeInternal))
	}
	w := &fakeDropWriter{}
	r := newDropReporter(w, src)
	r.sent(1)
	before := testutil.ToFloat64(metricSubscriberDropsLostTotal)

	r.flush()

	assert.Equal(t, []moqt.SubscribeDrop{dropRange(5, 5, codeInternal), dropRange(6, 6, codeInternal)}, w.sent)
	assert.InDelta(t, 3, testutil.ToFloat64(metricSubscriberDropsLostTotal)-before, 0)
}

// A skip that overlaps an upstream drop already forwarded sends only the
// groups upstream did not name, so no group is counted twice.
func TestDropReporter_ReportSkipped_AfterUpstreamDrop(t *testing.T) {
	src := newDropDistributor(4)
	src.record(dropRange(5, 7, codeInternal))
	w := &fakeDropWriter{}
	r := newDropReporter(w, src)
	r.sent(4)
	r.flush()

	r.reportSkipped(10, 10, codeTimeout)

	assert.Equal(t, []moqt.SubscribeDrop{dropRange(5, 7, codeInternal), dropRange(8, 9, codeTimeout)}, w.sent)
}

func TestMergeSpans(t *testing.T) {
	tests := map[string]struct {
		in   []groupSpan
		want []groupSpan
	}{
		"empty":    {in: nil, want: nil},
		"single":   {in: []groupSpan{{3, 4}}, want: []groupSpan{{3, 4}}},
		"disjoint": {in: []groupSpan{{6, 7}, {1, 2}}, want: []groupSpan{{1, 2}, {6, 7}}},
		"adjacent": {in: []groupSpan{{1, 2}, {3, 4}}, want: []groupSpan{{1, 4}}},
		"overlap":  {in: []groupSpan{{1, 5}, {3, 8}}, want: []groupSpan{{1, 8}}},
		"contained": {
			in:   []groupSpan{{1, 10}, {3, 4}},
			want: []groupSpan{{1, 10}},
		},
		"chain": {
			in:   []groupSpan{{9, 9}, {1, 3}, {4, 6}, {12, 13}, {7, 8}},
			want: []groupSpan{{1, 9}, {12, 13}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := mergeSpans(tt.in)
			if tt.want == nil {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDropReporter_Sent(t *testing.T) {
	tests := map[string]struct {
		sent []moqt.GroupSequence
		want []groupSpan
	}{
		"in order merges into one span":       {sent: []moqt.GroupSequence{1, 2, 3}, want: []groupSpan{{1, 3}}},
		"gap keeps two spans":                 {sent: []moqt.GroupSequence{1, 2, 5}, want: []groupSpan{{1, 2}, {5, 5}}},
		"late group fills the gap":            {sent: []moqt.GroupSequence{1, 3, 2}, want: []groupSpan{{1, 3}}},
		"late group below every span":         {sent: []moqt.GroupSequence{5, 6, 2}, want: []groupSpan{{2, 2}, {5, 6}}},
		"repeat of the newest group":          {sent: []moqt.GroupSequence{1, 2, 2}, want: []groupSpan{{1, 2}}},
		"group inside the newest span":        {sent: []moqt.GroupSequence{4, 5, 6, 5}, want: []groupSpan{{4, 6}}},
		"late group joins two earlier spans":  {sent: []moqt.GroupSequence{1, 3, 5, 4, 2}, want: []groupSpan{{1, 5}}},
		"late group touching the newest span": {sent: []moqt.GroupSequence{1, 4, 3}, want: []groupSpan{{1, 1}, {3, 4}}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := newDropReporter(&fakeDropWriter{}, newDropDistributor(4))
			for _, seq := range tt.sent {
				r.sent(seq)
			}

			assert.Equal(t, tt.want, r.handled)
			assert.Equal(t, tt.sent[0], r.start)
		})
	}
}
