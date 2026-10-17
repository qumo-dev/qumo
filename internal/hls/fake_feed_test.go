package hls

import (
	"context"
	"errors"
	"io"

	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/okdaichi/qumo-ledger/ledger"
)

// Fakes for the feed's MoQ and ledger seams. feedMedia and feedAnnouncedTrack
// depend on [mediaSubscriber]/[groupFeeder]/[receivedGroup] rather than a
// concrete MoQ session, and on [groupAppender]/[epochAppender] rather than the
// ledger writer, precisely so
// these can drive its orchestration — skip, advance, time out — without a QUIC
// relay or a store.

var (
	_ mediaSubscriber = (*fakeSubscriber)(nil)
	_ groupFeeder     = (*fakeFeeder)(nil)
	_ receivedGroup   = (*fakeGroup)(nil)
	_ epochAppender   = (*fakeAppender)(nil)
)

// Sentinels a test threads through a fake to assert feedMedia's handling.
var (
	// errFeederDone ends a feed once its queued groups run out, so an
	// orchestration test returns instead of looping forever. It is not the
	// publisher-gone timeout, which the feeder signals by blocking instead.
	errFeederDone = errors.New("feeder exhausted")
	// errSubscribe stands in for a relay refusing the media subscription.
	errSubscribe = errors.New("subscribe refused")
)

// subscribeResult is one SubscribeMedia outcome: a feeder, or an error.
type subscribeResult struct {
	feeder groupFeeder
	err    error
}

// fakeSubscriber answers each subscribe from results in order; once the queue
// is down to its last entry, that entry repeats for every further call. With
// no results it hands out feeder, or err when set. subscribes counts the calls.
type fakeSubscriber struct {
	results    []subscribeResult
	feeder     groupFeeder
	err        error
	subscribes int
}

func (s *fakeSubscriber) SubscribeMedia(context.Context, mediaInfo) (groupFeeder, error) {
	s.subscribes++
	if len(s.results) > 0 {
		r := s.results[0]
		if len(s.results) > 1 {
			s.results = s.results[1:]
		}
		return r.feeder, r.err
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.feeder, nil
}

// acceptResult is one AcceptGroup outcome: a group to read, or an error the feed
// treats as an accept failure.
type acceptResult struct {
	group receivedGroup
	err   error
}

// fakeFeeder serves queued groups in order. Once the queue empties it returns
// tailErr (ending a synchronous test) or, when tailErr is nil, blocks on the
// accept context until it expires — the publisher-gone timeout path.
type fakeFeeder struct {
	groups []acceptResult
	tail   error
	closed bool
}

func (f *fakeFeeder) AcceptGroup(ctx context.Context) (receivedGroup, error) {
	if len(f.groups) > 0 {
		r := f.groups[0]
		f.groups = f.groups[1:]
		return r.group, r.err
	}
	if f.tail != nil {
		return nil, f.tail
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *fakeFeeder) Close() error { f.closed = true; return nil }

// fakeGroup is one received group: a producer sequence and a run of frame
// bodies, each already in wire form — LOC-encoded for a group the feed should
// accept, or truncated bytes for one drainGroup should reject.
type fakeGroup struct {
	seq    moqt.GroupSequence
	bodies [][]byte
	idx    int
}

func (g *fakeGroup) GroupSequence() moqt.GroupSequence { return g.seq }

func (g *fakeGroup) ReadFrame(f *moqt.Frame) error {
	if g.idx >= len(g.bodies) {
		return io.EOF
	}
	f.Reset()
	_, _ = f.Write(g.bodies[g.idx])
	g.idx++
	return nil
}

// fakeAppender records what the feed appended and can refuse a specific call to
// exercise the skip-on-append path. A call succeeds unless its entry in errs is
// non-nil; an exhausted errs queue means unconditional success. Each NewEpoch
// records how many groups had been appended when it ran (epochMarks), so a
// test can tell which groups each closed epoch holds; it fails with epochErr
// when set.
type fakeAppender struct {
	appended   []ledger.GroupInfo
	payloads   [][]byte
	errs       []error
	epochMarks []int
	epochErr   error
}

func (a *fakeAppender) AppendGroup(_ context.Context, meta ledger.GroupInfo, payload []byte) (ledger.GroupInfo, error) {
	var err error
	if len(a.errs) > 0 {
		err = a.errs[0]
		a.errs = a.errs[1:]
	}
	if err != nil {
		return ledger.GroupInfo{}, err
	}
	a.appended = append(a.appended, meta)
	a.payloads = append(a.payloads, append([]byte(nil), payload...))
	return meta, nil
}

func (a *fakeAppender) NewEpoch(context.Context) error {
	if a.epochErr != nil {
		return a.epochErr
	}
	a.epochMarks = append(a.epochMarks, len(a.appended))
	return nil
}
