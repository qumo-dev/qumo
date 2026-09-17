package ingest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// sourceGroup tests
// ---------------------------------------------------------------------------

func TestSourceGroup_Append_Next(t *testing.T) {
	g := &sourceGroup{
		seq:    1,
		frames: make([]*moqt.Frame, 0, 4),
	}

	f1 := moqt.NewFrame(5)
	f1.Write([]byte("hello"))
	f2 := moqt.NewFrame(5)
	f2.Write([]byte("world"))

	g.append(f1)
	g.append(f2)

	assert.Equal(t, f1, g.next(0))
	assert.Equal(t, f2, g.next(1))
	assert.Nil(t, g.next(2))
	assert.Nil(t, g.next(-1))
}

func TestSourceGroup_IsComplete(t *testing.T) {
	g := &sourceGroup{seq: 1, frames: make([]*moqt.Frame, 0, 4)}

	assert.False(t, g.isComplete())

	g.complete.Store(true)
	assert.True(t, g.isComplete())
}

// ---------------------------------------------------------------------------
// trackBuffer tests
// ---------------------------------------------------------------------------

func newTestTrackBuffer() *trackBuffer {
	return &trackBuffer{
		name:        "test",
		ring:        make([]atomic.Pointer[sourceGroup], defaultRingSize),
		size:        defaultRingSize,
		subscribers: make(map[chan struct{}]struct{}),
	}
}

func TestTrackBuffer_OpenGroup(t *testing.T) {
	b := newTestTrackBuffer()

	assert.Equal(t, moqt.GroupSequence(0), b.head())

	// Create first group.
	g1 := b.openGroup()
	assert.Equal(t, moqt.GroupSequence(1), g1.seq)
	assert.Equal(t, moqt.GroupSequence(1), b.head())

	// Create second group.
	g2 := b.openGroup()
	assert.Equal(t, moqt.GroupSequence(2), g2.seq)
	assert.Equal(t, moqt.GroupSequence(2), b.head())
}

func TestTrackBuffer_Get(t *testing.T) {
	b := newTestTrackBuffer()

	g := b.openGroup()
	got := b.get(g.seq)
	assert.Equal(t, g, got)
}

func TestTrackBuffer_EarliestAvailable(t *testing.T) {
	b := newTestTrackBuffer()

	// No groups yet — earliest is still 1 (since head=0 < size=8).
	assert.Equal(t, moqt.GroupSequence(1), b.earliestAvailable())

	// Add groups up to ring size.
	for range defaultRingSize {
		b.openGroup()
	}
	// head = 8, size = 8, 8 <= 8 → earliest = 1
	assert.Equal(t, moqt.GroupSequence(1), b.earliestAvailable())

	// Add one more to overflow the ring.
	b.openGroup()
	// head = 9, 9 > 8 → earliest = 9 - 8 + 1 = 2
	assert.Equal(t, moqt.GroupSequence(2), b.earliestAvailable())
}

// ---------------------------------------------------------------------------
// videoTrack tests
// ---------------------------------------------------------------------------

func TestVideoTrack_Push(t *testing.T) {
	v := newVideoTrack(context.Background())

	// First push creates a new group (keyframe).
	f1 := moqt.NewFrame(4)
	f1.Write([]byte{0x17, 0x01, 0x00, 0x00})
	v.push(f1, true, 1000)

	assert.Equal(t, moqt.GroupSequence(1), v.buf.head())
	g := v.buf.get(1)
	require.NotNil(t, g)
	assert.Equal(t, f1, g.next(0))
	assert.False(t, g.isComplete())

	// Non-keyframe appends to current group.
	f2 := moqt.NewFrame(3)
	f2.Write([]byte{0x27, 0x01, 0x00})
	v.push(f2, false, 2000)

	assert.Equal(t, moqt.GroupSequence(1), v.buf.head()) // same group
	assert.Equal(t, f2, g.next(1))

	// New keyframe starts a new group and completes the old one.
	f3 := moqt.NewFrame(4)
	f3.Write([]byte{0x17, 0x01, 0x00, 0x00})
	v.push(f3, true, 3000)

	assert.Equal(t, moqt.GroupSequence(2), v.buf.head())
	assert.True(t, g.isComplete()) // old group completed
	g2 := v.buf.get(2)
	require.NotNil(t, g2)
	assert.Equal(t, f3, g2.next(0))
}

// TestVideoTrack_Push_SameTimestampKeyframesCollapses verifies the #229 fix: a
// publisher that emits several keyframe NALUs at the same presentation time
// (e.g. ffmpeg's RTSP muxer emitting redundant IDRs in one access unit) must
// not open a fresh MoQT group for each — they belong to one access unit, so one
// group. Opening one group per redundant IDR caused rapid micro-group churn
// that the relay ring / bounded collector window delivered out of order,
// observed downstream as a spurious PTS regression.
func TestVideoTrack_Push_SameTimestampKeyframesCollapses(t *testing.T) {
	v := newVideoTrack(context.Background())

	// First keyframe opens group 1.
	k1 := moqt.NewFrame(4)
	k1.Write([]byte{0x17, 0x01, 0x00, 0x00})
	v.push(k1, true, 5000)

	// Two more keyframes at the SAME timestamp must NOT open new groups.
	k2 := moqt.NewFrame(4)
	k2.Write([]byte{0x17, 0x01, 0x00, 0x00})
	v.push(k2, true, 5000)
	k3 := moqt.NewFrame(4)
	k3.Write([]byte{0x17, 0x01, 0x00, 0x00})
	v.push(k3, true, 5000)

	assert.Equal(t, moqt.GroupSequence(1), v.buf.head(), "same-timestamp keyframes should share one group")
	g := v.buf.get(1)
	require.NotNil(t, g)
	assert.Equal(t, k1, g.next(0))
	assert.Equal(t, k2, g.next(1))
	assert.Equal(t, k3, g.next(2))

	// A keyframe at a new timestamp opens a new group.
	k4 := moqt.NewFrame(4)
	k4.Write([]byte{0x17, 0x01, 0x00, 0x00})
	v.push(k4, true, 6000)
	assert.Equal(t, moqt.GroupSequence(2), v.buf.head())
}

// ---------------------------------------------------------------------------
// singleTrack tests
// ---------------------------------------------------------------------------

func TestSingleTrack_Push(t *testing.T) {
	s := newSingleTrack(context.Background())

	f := moqt.NewFrame(3)
	f.Write([]byte{0xAF, 0x01, 0x00})
	s.push(f)

	assert.Equal(t, moqt.GroupSequence(1), s.buf.head())
	g := s.buf.get(1)
	require.NotNil(t, g)
	assert.Equal(t, f, g.next(0))
	assert.True(t, g.isComplete()) // single-track groups are immediately complete
}

// TestSingleTrack_PushFrames covers the multi-frame coalescing path used by the
// RTSP audio ingest: one source packet carrying N AAC frames becomes ONE MoQT
// group with N frames. Pushing N separate groups would burst N concurrent QUIC
// streams that arrive out of order; one group keeps them on a single stream.
func TestSingleTrack_PushFrames(t *testing.T) {
	s := newSingleTrack(context.Background())

	frames := []*moqt.Frame{
		moqt.NewFrame(2), moqt.NewFrame(2), moqt.NewFrame(2),
	}
	for _, f := range frames {
		f.Write([]byte{0xAA, 0x01})
	}
	s.pushFrames(frames)

	// Exactly one group holds all three frames, in order.
	assert.Equal(t, moqt.GroupSequence(1), s.buf.head())
	g := s.buf.get(1)
	require.NotNil(t, g)
	assert.True(t, g.isComplete())
	for i, f := range frames {
		assert.Equal(t, f, g.next(i), "frame %d must be in the single group at index %d", i, i)
	}
	assert.Nil(t, g.next(len(frames)), "no extra frames")
}

func TestSingleTrack_PushFrames_Empty(t *testing.T) {
	s := newSingleTrack(context.Background())
	s.pushFrames(nil) // must not panic or open a group
	assert.Equal(t, moqt.GroupSequence(0), s.buf.head())
}

func TestVideoTrack_Close(t *testing.T) {
	v := newVideoTrack(context.Background())

	// Push a video frame to create a group.
	f := moqt.NewFrame(3)
	f.Write([]byte{0x17, 0x01, 0x00})
	v.push(f, true, 1000)

	g := v.buf.get(1)
	require.NotNil(t, g)
	assert.False(t, g.isComplete())

	v.close()
	assert.True(t, g.isComplete())
}

func TestVideoTrack_Close_NoGroup(t *testing.T) {
	v := newVideoTrack(context.Background())
	// Should not panic when no current group exists.
	v.close()
}

func TestTrackBuffer_SubscribeUnsubscribe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newTestTrackBuffer()

		ch1 := b.subscribe()
		ch2 := b.subscribe()

		// Both subscribers receive notifications.
		b.notify()
		synctest.Wait()

		select {
		case <-ch1:
		default:
			t.Fatal("subscriber 1 did not receive notification")
		}
		select {
		case <-ch2:
		default:
			t.Fatal("subscriber 2 did not receive notification")
		}

		// Unsubscribe ch1; only ch2 should receive further notifications.
		b.unsubscribe(ch1)

		b.notify()
		synctest.Wait()

		select {
		case <-ch1:
			t.Fatal("unsubscribed channel should not receive notification")
		default:
		}
		select {
		case <-ch2:
		default:
			t.Fatal("subscriber 2 did not receive notification after ch1 unsubscribed")
		}
	})
}

func TestTrackBuffer_Notify(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newTestTrackBuffer()

		ch := b.subscribe()
		defer b.unsubscribe(ch)

		b.notify()

		synctest.Wait()

		select {
		case <-ch:
			// Expected: notification received.
		default:
			t.Fatal("expected notification on subscriber channel")
		}
	})
}

func TestTrackBuffer_Notify_NonBlocking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newTestTrackBuffer()

		ch := b.subscribe()
		defer b.unsubscribe(ch)

		// Fill the channel buffer.
		b.notify()
		synctest.Wait()

		// Second notify should not block.
		b.notify()
		synctest.Wait()

		// Drain one.
		<-ch

		// Channel should be empty now.
		select {
		case <-ch:
			t.Fatal("expected no more notifications")
		default:
		}
	})
}

func TestTrackBuffer_MultipleSubscribers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newTestTrackBuffer()

		ch1 := b.subscribe()
		ch2 := b.subscribe()
		defer b.unsubscribe(ch1)
		defer b.unsubscribe(ch2)

		b.notify()
		synctest.Wait()

		select {
		case <-ch1:
		default:
			t.Fatal("subscriber 1 did not receive notification")
		}

		select {
		case <-ch2:
		default:
			t.Fatal("subscriber 2 did not receive notification")
		}
	})
}

func TestTrackBuffer_RingWrapAround(t *testing.T) {
	b := newTestTrackBuffer()

	// Fill beyond ring size.
	for i := range defaultRingSize + 3 {
		g := b.openGroup()
		f := moqt.NewFrame(1)
		f.Write([]byte{byte(i)})
		g.append(f)
		g.complete.Store(true)
	}

	// Oldest slots have been overwritten.
	head := b.head()
	assert.Equal(t, moqt.GroupSequence(defaultRingSize+3), head)

	// Can still access recent groups.
	for seq := b.earliestAvailable(); seq <= head; seq++ {
		g := b.get(seq)
		require.NotNil(t, g)
		assert.Equal(t, seq, g.seq)
	}
}

// ---------------------------------------------------------------------------
// ingestHandler tests
// ---------------------------------------------------------------------------

func TestNewIngestHandler(t *testing.T) {
	h, err := newIngestHandler(context.Background())
	require.NoError(t, err)

	require.NotNil(t, h.broadcast)
	require.NotNil(t, h.video)
	require.NotNil(t, h.audio)
}

func TestIngestHandler_Close(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		// No defer cancel() here, we do it explicitly to wait.

		h, err := newIngestHandler(ctx)
		require.NoError(t, err)

		// Push a video frame so there's a current group.
		f := moqt.NewFrame(3)
		f.Write([]byte{0x17, 0x01, 0x00})
		h.video.push(f, true, 1000)

		g := h.video.buf.get(1)
		require.NotNil(t, g)
		assert.False(t, g.isComplete())

		h.close()

		// Video group completed.
		assert.True(t, g.isComplete())

		// Context cancelled by caller (Session) — simulate.
		cancel()

		// Wait for pollCacheDepth goroutines to see the cancellation and exit.
		synctest.Wait()

		select {
		case <-h.video.ctx.Done():
		default:
			t.Fatal("expected context to be cancelled")
		}
	})
}

func TestIngestHandler_Close_Idempotent(t *testing.T) {
	h, err := newIngestHandler(context.Background())
	require.NoError(t, err)

	// Multiple calls should not panic.
	h.close()
	h.close()
	h.close()
}

// ---------------------------------------------------------------------------
// Session tests
// ---------------------------------------------------------------------------

func TestNewSession_PushVideo_PushAudio_Close(t *testing.T) {
	mux := moqt.NewTrackMux(0)

	sess, err := NewSession(mux, "/live/test")
	require.NoError(t, err)

	// Push video keyframe.
	sess.PushVideo(0, []byte{0x17, 0x01, 0x00, 0x00}, true)
	assert.Equal(t, moqt.GroupSequence(1), sess.handler.video.buf.head())

	// Push video inter-frame (same group).
	sess.PushVideo(33000, []byte{0x27, 0x01, 0x00, 0x00}, false)
	assert.Equal(t, moqt.GroupSequence(1), sess.handler.video.buf.head())

	// Push audio.
	sess.PushAudio(0, []byte{0xAF, 0x01, 0x00})
	assert.Equal(t, moqt.GroupSequence(1), sess.handler.audio.buf.head())

	// Close should not panic.
	sess.Close()
}

func TestSession_Close_Idempotent(t *testing.T) {
	mux := moqt.NewTrackMux(0)
	sess, err := NewSession(mux, "/live/test2")
	require.NoError(t, err)

	sess.Close()
	sess.Close() // should not panic
}

func TestSession_PushMultipleKeyframes(t *testing.T) {
	mux := moqt.NewTrackMux(0)
	sess, err := NewSession(mux, "/live/gop-test")
	require.NoError(t, err)
	defer sess.Close()

	// First keyframe → group 1.
	sess.PushVideo(0, []byte{0x17, 0x01}, true)
	assert.Equal(t, moqt.GroupSequence(1), sess.handler.video.buf.head())

	// Inter-frames in group 1.
	sess.PushVideo(33000, []byte{0x27, 0x01}, false)
	sess.PushVideo(66000, []byte{0x27, 0x02}, false)

	// Second keyframe → group 2.
	sess.PushVideo(100000, []byte{0x17, 0x02}, true)
	assert.Equal(t, moqt.GroupSequence(2), sess.handler.video.buf.head())

	// Verify group 1 was completed.
	g := sess.handler.video.buf.get(1)
	require.NotNil(t, g)
	assert.True(t, g.isComplete())

	// Verify group 2 has the keyframe (MediaFrame-wrapped).
	g2 := sess.handler.video.buf.get(2)
	require.NotNil(t, g2)
	f := g2.next(0)
	require.NotNil(t, f)
	expected := buildMediaFrame(100000, []byte{0x17, 0x02})
	assert.Equal(t, expected, f.Body())
}

func TestSession_AudioIndependentGroups(t *testing.T) {
	mux := moqt.NewTrackMux(0)
	sess, err := NewSession(mux, "/live/audio-test")
	require.NoError(t, err)
	defer sess.Close()

	// Each audio push creates its own complete group.
	sess.PushAudio(0, []byte{0xAF, 0x01})
	sess.PushAudio(23000, []byte{0xAF, 0x02})
	sess.PushAudio(46000, []byte{0xAF, 0x03})

	assert.Equal(t, moqt.GroupSequence(3), sess.handler.audio.buf.head())

	// All groups should be complete.
	for seq := moqt.GroupSequence(1); seq <= 3; seq++ {
		g := sess.handler.audio.buf.get(seq)
		require.NotNil(t, g)
		assert.True(t, g.isComplete())
	}
}

// TestSession_PushAudioFrames covers the coalesced-audio path: all frames in one
// call land in a SINGLE MoQT group (one QUIC stream), preserving their order.
// This is the RTSP AAC ingest fix for the multi-AU-per-packet burst that
// delivered audio out of PTS order and popped.
func TestSession_PushAudioFrames(t *testing.T) {
	mux := moqt.NewTrackMux(0)
	sess, err := NewSession(mux, "/live/audio-coalesce")
	require.NoError(t, err)
	defer sess.Close()

	// Three AAC frames from one RTP packet → one group, not three.
	sess.PushAudioFrames(
		[]int64{0, 21333, 42666},
		[][]byte{{0xAF, 0x01}, {0xAF, 0x02}, {0xAF, 0x03}},
	)

	assert.Equal(t, moqt.GroupSequence(1), sess.handler.audio.buf.head(), "one coalesced group")
	g := sess.handler.audio.buf.get(1)
	require.NotNil(t, g)
	assert.True(t, g.isComplete())
	require.Len(t, g.frames, 3)
	for i := range 3 {
		assert.NotNil(t, g.next(i), "frame %d present", i)
	}

	t.Run("mismatched lengths is a no-op", func(t *testing.T) {
		sess.PushAudioFrames([]int64{0}, [][]byte{{0x01}, {0x02}}) // 1 ts, 2 frames
		// No new group opened.
		assert.Equal(t, moqt.GroupSequence(1), sess.handler.audio.buf.head())
	})

	t.Run("empty is a no-op", func(t *testing.T) {
		sess.PushAudioFrames(nil, nil)
		assert.Equal(t, moqt.GroupSequence(1), sess.handler.audio.buf.head())
	})
}

// ---------------------------------------------------------------------------
// sourceGroup concurrency test
// ---------------------------------------------------------------------------

func TestSourceGroup_ConcurrentAppendNext(t *testing.T) {
	g := &sourceGroup{
		seq:    1,
		frames: make([]*moqt.Frame, 0, 100),
	}

	const n = 50
	var wg sync.WaitGroup

	wg.Go(func() {
		for i := range n {
			f := moqt.NewFrame(1)
			f.Write([]byte{byte(i)})
			g.append(f)
		}
	})

	wg.Go(func() {
		for i := range n {
			for {
				if f := g.next(i); f != nil {
					break
				}
			}
		}
	})

	wg.Wait()

	// All frames should be present after completion.
	for i := range n {
		assert.NotNil(t, g.next(i))
	}
	assert.Nil(t, g.next(n))
}

// ---------------------------------------------------------------------------
// registerVideo / registerAudio tests
// ---------------------------------------------------------------------------

func TestRegisterVideo(t *testing.T) {
	h, err := newIngestHandler(context.Background())
	require.NoError(t, err)

	cfg := &AVCConfig{
		ProfileIDC:    0x64,
		ProfileCompat: 0x00,
		LevelIDC:      0x1F,
		NALULenSize:   4,
		SPS:           [][]byte{{0x67, 0x64, 0x00, 0x1F, 0xAC}},
		PPS:           [][]byte{{0x68, 0xEB}},
		Width:         1920,
		Height:        1080,
	}

	require.NoError(t, h.registerVideo(cfg))

	data, err := h.broadcast.CatalogBytes()
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &raw))

	var tracks []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw["tracks"], &tracks))
	require.Len(t, tracks, 1)

	var name string
	require.NoError(t, json.Unmarshal(tracks[0]["name"], &name))
	assert.Equal(t, "video", name)

	var codec string
	require.NoError(t, json.Unmarshal(tracks[0]["codec"], &codec))
	assert.Equal(t, "avc1.64001f", codec)

	// initRef points at the catalog's InitDataList entry carrying the
	// Base64-encoded AVCDecoderConfigurationRecord.
	var initRef string
	require.NoError(t, json.Unmarshal(tracks[0]["initRef"], &initRef))
	decoded, err := base64.StdEncoding.DecodeString(initDataByID(t, raw, initRef))
	require.NoError(t, err)
	require.NotEmpty(t, decoded)
	assert.Equal(t, byte(0x01), decoded[0], "configurationVersion")
}

// initDataByID looks up the base64 Data of the InitDataList entry with the
// given id from a parsed catalog's raw JSON.
func initDataByID(t *testing.T, raw map[string]json.RawMessage, id string) string {
	t.Helper()
	var entries []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw["initDataList"], &entries))
	for _, entry := range entries {
		var entryID string
		require.NoError(t, json.Unmarshal(entry["id"], &entryID))
		if entryID == id {
			var data string
			require.NoError(t, json.Unmarshal(entry["data"], &data))
			return data
		}
	}
	t.Fatalf("initDataList entry %q not found", id)
	return ""
}

func TestRegisterAudio(t *testing.T) {
	h, err := newIngestHandler(context.Background())
	require.NoError(t, err)

	cfg := &AACConfig{
		ObjectType:    2,
		SampleRate:    48000,
		ChannelConfig: 2,
	}

	require.NoError(t, h.registerAudio(cfg))

	data, err := h.broadcast.CatalogBytes()
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &raw))

	var tracks []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw["tracks"], &tracks))
	require.Len(t, tracks, 1)

	var name string
	require.NoError(t, json.Unmarshal(tracks[0]["name"], &name))
	assert.Equal(t, "audio", name)

	var codec string
	require.NoError(t, json.Unmarshal(tracks[0]["codec"], &codec))
	assert.Equal(t, "mp4a.40.2", codec)

	// initRef points at the catalog's InitDataList entry carrying the
	// Base64-encoded AudioSpecificConfig (2 bytes for AAC-LC).
	var initRef string
	require.NoError(t, json.Unmarshal(tracks[0]["initRef"], &initRef))
	decoded, err := base64.StdEncoding.DecodeString(initDataByID(t, raw, initRef))
	require.NoError(t, err)
	assert.Len(t, decoded, 2)
}

func TestRegisterVideoAndAudio(t *testing.T) {
	h, err := newIngestHandler(context.Background())
	require.NoError(t, err)

	video := &AVCConfig{
		ProfileIDC:    0x64,
		ProfileCompat: 0x00,
		LevelIDC:      0x1F,
		NALULenSize:   4,
		SPS:           [][]byte{{0x67, 0x64, 0x00, 0x1F, 0xAC}},
		PPS:           [][]byte{{0x68, 0xEB}},
		Width:         1920,
		Height:        1080,
	}
	audio := &AACConfig{
		ObjectType:    2,
		SampleRate:    48000,
		ChannelConfig: 2,
	}

	require.NoError(t, h.registerVideo(video))
	require.NoError(t, h.registerAudio(audio))

	data, err := h.broadcast.CatalogBytes()
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &raw))

	var tracks []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw["tracks"], &tracks))
	assert.Len(t, tracks, 2)
}

func TestIngestHandler_TrackInfo(t *testing.T) {
	h, err := newIngestHandler(context.Background())
	require.NoError(t, err)

	var tip moqt.TrackInfoProvider = h

	// Test video track info
	vInfo, ok := tip.TrackInfo("video")
	assert.True(t, ok)
	assert.Equal(t, moqt.TrackPriority(128), vInfo.Priority)
	assert.True(t, vInfo.Ordered)
	assert.Equal(t, uint64(2000), vInfo.MaxLatency)
	assert.Equal(t, uint64(1_000_000), vInfo.Timescale)

	// Test audio track info
	aInfo, ok := tip.TrackInfo("audio")
	assert.True(t, ok)
	assert.Equal(t, moqt.TrackPriority(128), aInfo.Priority)
	assert.False(t, aInfo.Ordered)
	assert.Equal(t, uint64(2000), aInfo.MaxLatency)
	assert.Equal(t, uint64(1_000_000), aInfo.Timescale)

	// Test catalog track info
	cInfo, ok := tip.TrackInfo(h.broadcast.CatalogTrackName())
	assert.True(t, ok)
	assert.Equal(t, moqt.TrackPriority(255), cInfo.Priority)
	assert.True(t, cInfo.Ordered)
	assert.Equal(t, uint64(1000), cInfo.Timescale)

	// Unknown track
	_, ok = tip.TrackInfo("unknown")
	assert.False(t, ok)
}
