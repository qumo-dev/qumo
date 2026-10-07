package funnel

import (
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// subscribe registers a subscriber channel on t as serve does, without a
// MoQ session.
func subscribe(tb testing.TB, t *track) chan group {
	tb.Helper()
	ch := make(chan group, subscriberBuffer)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.subscribers[ch] = struct{}{}
	return ch
}

func TestEgress_Announce(t *testing.T) {
	mux := moqt.NewTrackMux(0)
	e := newEgress(t.Context(), mux)

	first := e.announce("/room/123")
	again := e.announce("/room/123")
	other := e.announce("/room/9")

	assert.Same(t, first, again, "announcing a path again reuses its broadcast")
	assert.NotSame(t, first, other)
	ann, handler := mux.TrackHandler("/room/123")
	require.NotNil(t, ann, "the path is published on the mux")
	assert.Same(t, first, handler)
}

func TestEgress_Publish_AnnouncesAnUnannouncedPath(t *testing.T) {
	mux := moqt.NewTrackMux(0)
	e := newEgress(t.Context(), mux)

	e.publish("/room/123", "chat", group{seq: 1, payload: []byte(`"hi"`)})

	_, handler := mux.TrackHandler("/room/123")
	require.NotNil(t, handler)
	latest := handler.(*broadcast).track("chat").latest
	require.NotNil(t, latest)
	assert.Equal(t, group{seq: 1, payload: []byte(`"hi"`)}, *latest)
}

func TestBroadcast_Track_IsCreatedOnceByName(t *testing.T) {
	b := &broadcast{tracks: make(map[moqt.TrackName]*track)}

	chat := b.track("chat")

	assert.Same(t, chat, b.track("chat"))
	assert.NotSame(t, chat, b.track("reactions"))
}

func TestTrack_Publish(t *testing.T) {
	tests := map[string]struct {
		subscribers int
		records     int
		wantEach    int
	}{
		"no subscriber keeps only the latest": {subscribers: 0, records: 3},
		"every subscriber receives each":      {subscribers: 3, records: 5, wantEach: 5},
		"a full buffer misses the overflow":   {subscribers: 1, records: subscriberBuffer + 10, wantEach: subscriberBuffer},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			tr := &track{subscribers: make(map[chan group]struct{})}
			chans := make([]chan group, tt.subscribers)
			for i := range chans {
				chans[i] = subscribe(t, tr)
			}

			for i := range tt.records {
				tr.publish(group{seq: moqt.GroupSequence(i + 1)})
			}

			require.NotNil(t, tr.latest)
			assert.Equal(t, moqt.GroupSequence(tt.records), tr.latest.seq, "the latest record is kept for late joiners")
			for _, ch := range chans {
				require.Len(t, ch, tt.wantEach)
				assert.Equal(t, moqt.GroupSequence(1), (<-ch).seq, "a subscriber receives records in order from the first")
			}
		})
	}
}
