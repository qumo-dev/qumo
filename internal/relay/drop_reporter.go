package relay

import (
	"log/slog"
	"slices"

	"github.com/qumo-dev/gomoqt/moqt"
)

// maxHandledSpans caps how many separate ranges a dropReporter keeps.
// Adjacent ranges merge, so a subscriber that keeps up holds one span; the cap
// is reached only when many isolated groups are dropped or arrive far out of
// order.
const maxHandledSpans = 64

// dropWriter sends SUBSCRIBE_DROP to one subscriber. *moqt.TrackWriter
// implements it.
type dropWriter interface {
	DropGroups(drop moqt.SubscribeDrop) error
}

var _ dropWriter = (*moqt.TrackWriter)(nil)

// groupSpan is an inclusive range of group sequences.
type groupSpan struct {
	start, end moqt.GroupSequence
}

// dropReporter tells one subscriber which groups it will not receive: drops
// forwarded from upstream, groups the relay skipped because the subscriber
// fell behind, and groups whose stream could not be opened in time.
//
// It keeps the set of groups already handled, sent or reported dropped, so a
// group is reported at most once, whichever source names it first, and a group
// already reported is never sent afterwards. It is used only by the
// subscriber's egress goroutine and is not safe for concurrent use.
type dropReporter struct {
	w   dropWriter
	src *dropDistributor

	// pos is this subscriber's read position in src.
	pos uint64
	buf []moqt.SubscribeDrop

	// started is set once the first group is sent; start is its sequence.
	// gomoqt resolves the subscription's start to the first group opened, and
	// before that it folds DropGroups into SUBSCRIBE_OK instead of sending
	// SUBSCRIBE_DROP. So nothing is reported before start is known, and ranges
	// below start are trimmed: the subscriber does not wait for them.
	started bool
	start   moqt.GroupSequence

	// handled holds the groups sent or reported, sorted, with no overlapping
	// or adjacent spans.
	handled []groupSpan
}

func newDropReporter(w dropWriter, src *dropDistributor) *dropReporter {
	return &dropReporter{w: w, src: src}
}

// sent records that group seq was delivered. The first call sets the
// subscription's start.
func (r *dropReporter) sent(seq moqt.GroupSequence) {
	if !r.started {
		r.started = true
		r.start = seq
	}
	r.add(groupSpan{start: seq, end: seq})
}

// report sends drop for the groups in it that are not handled yet and marks
// them handled. It does nothing before the first group is sent.
func (r *dropReporter) report(drop moqt.SubscribeDrop) {
	if !r.started || drop.StartGroup > drop.EndGroup || drop.EndGroup < r.start {
		return
	}
	// Group 0 is never a valid drop bound in gomoqt.
	drop.StartGroup = max(drop.StartGroup, r.start, moqt.GroupSequence(1))
	if drop.StartGroup > drop.EndGroup {
		return
	}

	span := groupSpan{start: drop.StartGroup, end: drop.EndGroup}
	for _, s := range r.unhandled(span) {
		_ = r.w.DropGroups(moqt.SubscribeDrop{ // not actionable: a dead subscribe stream also fails the next group delivery, which ends egress
			StartGroup: s.start,
			EndGroup:   s.end,
			ErrorCode:  drop.ErrorCode,
		})
	}
	r.add(span)
}

// reportSkipped reports every group from the subscription's start to highest
// that is not handled yet, except next, the group egress sends after the skip.
func (r *dropReporter) reportSkipped(next, highest moqt.GroupSequence, code moqt.SubscribeErrorCode) {
	if !r.started {
		return
	}
	if next > r.start {
		r.report(moqt.SubscribeDrop{StartGroup: r.start, EndGroup: next - 1, ErrorCode: code})
	}
	if highest > next {
		r.report(moqt.SubscribeDrop{StartGroup: next + 1, EndGroup: highest, ErrorCode: code})
	}
}

// flush forwards the upstream drops recorded since the last flush. It does
// nothing before the first group is sent, so drops recorded earlier are read
// on the first flush after it.
func (r *dropReporter) flush() {
	if !r.started || r.src.head() == r.pos {
		return
	}

	var lost uint64
	r.buf, r.pos, lost = r.src.since(r.pos, r.buf[:0])
	if lost > 0 {
		metricSubscriberDropsLostTotal.Add(float64(lost))
		slog.Debug("subscriber fell behind upstream drop notifications; some were not forwarded",
			"lost", lost,
		)
	}
	for _, drop := range r.buf {
		r.report(drop)
	}
}

// isHandled reports whether seq was sent or reported dropped.
func (r *dropReporter) isHandled(seq moqt.GroupSequence) bool {
	_, found := slices.BinarySearchFunc(r.handled, seq, func(s groupSpan, seq moqt.GroupSequence) int {
		switch {
		case s.end < seq:
			return -1
		case s.start > seq:
			return 1
		}
		return 0
	})
	return found
}

// unhandled returns the parts of span not yet handled.
func (r *dropReporter) unhandled(span groupSpan) []groupSpan {
	var out []groupSpan
	next := span.start
	for _, s := range r.handled {
		if s.end < next {
			continue
		}
		if s.start > span.end {
			break
		}
		if s.start > next {
			out = append(out, groupSpan{start: next, end: s.start - 1})
		}
		if s.end >= span.end {
			return out
		}
		next = s.end + 1
	}
	return append(out, groupSpan{start: next, end: span.end})
}

// add marks span handled. Past maxHandledSpans the two oldest spans are joined,
// which also marks the gap between them handled: a group in that gap that
// arrives later is not sent, and one never reported is left to gomoqt's grace
// period, the same as before drops were reported at all.
func (r *dropReporter) add(span groupSpan) {
	// Fast path for the usual case, a group at or past the newest span: no
	// sort needed.
	if n := len(r.handled); n == 0 || span.start > r.handled[n-1].end+1 {
		r.handled = append(r.handled, span)
	} else if span.start >= r.handled[n-1].start {
		r.handled[n-1].end = max(r.handled[n-1].end, span.end)
	} else {
		r.handled = mergeSpans(append(r.handled, span))
	}
	if len(r.handled) > maxHandledSpans {
		r.handled[1].start = r.handled[0].start
		r.handled = slices.Delete(r.handled, 0, 1)
	}
}

// mergeSpans sorts spans and joins overlapping or adjacent ones, reusing the
// slice's storage.
func mergeSpans(spans []groupSpan) []groupSpan {
	slices.SortFunc(spans, func(a, b groupSpan) int {
		switch {
		case a.start < b.start:
			return -1
		case a.start > b.start:
			return 1
		}
		return 0
	})

	out := spans[:0]
	for _, s := range spans {
		if n := len(out); n > 0 && s.start <= out[n-1].end+1 {
			out[n-1].end = max(out[n-1].end, s.end)
			continue
		}
		out = append(out, s)
	}
	return out
}
