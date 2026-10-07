package funnel

import (
	"context"
	"log/slog"
	"sync"

	"github.com/qumo-dev/gomoqt/moqt"
)

// subscriberBuffer is how many records a subscriber may fall behind before it
// starts missing them.
const subscriberBuffer = 64

// egress serves recorded tracks to MoQ subscribers. Each announced broadcast
// path is published on the mux, and every record becomes one group of its
// track, numbered after the ledger group that stores it.
type egress struct {
	mux *moqt.TrackMux
	ctx context.Context

	mu         sync.Mutex
	broadcasts map[moqt.BroadcastPath]*broadcast
}

func newEgress(ctx context.Context, mux *moqt.TrackMux) *egress {
	return &egress{mux: mux, ctx: ctx, broadcasts: make(map[moqt.BroadcastPath]*broadcast)}
}

// announce publishes path on the mux. Announcing a path again does nothing.
func (e *egress) announce(path moqt.BroadcastPath) *broadcast {
	e.mu.Lock()
	defer e.mu.Unlock()
	if b, ok := e.broadcasts[path]; ok {
		return b
	}
	b := &broadcast{tracks: make(map[moqt.TrackName]*track)}
	e.broadcasts[path] = b
	e.mux.Publish(e.ctx, path, b)
	return b
}

// publish delivers one record to the subscribers of a track, announcing its
// broadcast first when nobody has.
func (e *egress) publish(path moqt.BroadcastPath, name moqt.TrackName, g group) {
	e.announce(path).track(name).publish(g)
}

// group is one record as it is sent: the sequence of the ledger group that
// stores it, and the stored bytes.
type group struct {
	seq     moqt.GroupSequence
	payload []byte
}

// broadcast is the tracks of one announced path. It implements
// [moqt.TrackHandler].
type broadcast struct {
	mu     sync.Mutex
	tracks map[moqt.TrackName]*track
}

// track returns the named track, creating it. Only an announce, a record or a
// restore creates tracks; a subscriber never does.
func (b *broadcast) track(name moqt.TrackName) *track {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.tracks[name]
	if !ok {
		t = &track{subscribers: make(map[chan group]struct{})}
		b.tracks[name] = t
	}
	return t
}

// lookup returns the named track if it exists.
func (b *broadcast) lookup(name moqt.TrackName) (*track, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.tracks[name]
	return t, ok
}

// ServeTrack sends a subscriber the track's latest record and then each new
// one, until the subscriber leaves. A track nobody announced is not found.
func (b *broadcast) ServeTrack(tw *moqt.TrackWriter) {
	t, ok := b.lookup(tw.TrackName)
	if !ok {
		tw.CloseWithError(moqt.SubscribeErrorCodeNotFound)
		return
	}
	t.serve(tw)
}

// track fans the records of one track out to its subscribers.
type track struct {
	mu          sync.Mutex
	latest      *group
	subscribers map[chan group]struct{}
}

// publish sends g to every subscriber. A subscriber whose buffer is full
// misses it.
func (t *track) publish(g group) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.latest = &g
	for ch := range t.subscribers {
		select {
		case ch <- g:
		default:
		}
	}
}

func (t *track) serve(tw *moqt.TrackWriter) {
	ch := make(chan group, subscriberBuffer)
	t.mu.Lock()
	if t.latest != nil {
		ch <- *t.latest
	}
	t.subscribers[ch] = struct{}{}
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.subscribers, ch)
		t.mu.Unlock()
	}()

	for {
		select {
		case <-tw.Context().Done():
			return
		case g := <-ch:
			if err := writeGroup(tw, g); err != nil {
				slog.Debug("funnel: subscriber ended", "broadcast_path", tw.BroadcastPath, "track_name", tw.TrackName, "error", err)
				return
			}
		}
	}
}

// writeGroup sends one record as a group holding a single frame.
func writeGroup(tw *moqt.TrackWriter, g group) error {
	gw, err := tw.OpenGroupAt(tw.Context(), g.seq)
	if err != nil {
		return err
	}
	frame := moqt.NewFrame(len(g.payload))
	if _, err := frame.Write(g.payload); err != nil {
		gw.CancelWrite(moqt.InternalGroupErrorCode)
		return err
	}
	if err := gw.WriteFrame(frame); err != nil {
		gw.CancelWrite(moqt.InternalGroupErrorCode)
		return err
	}
	return gw.Close()
}
