package ingest

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/gomoqt/msf"
)

const (
	defaultRingSize = 8

	notifyTimeout = 1 * time.Millisecond
)

var _ moqt.TrackHandler = (*ingestHandler)(nil)
var _ moqt.TrackInfoProvider = (*ingestHandler)(nil)

// ---------------------------------------------------------------------------
// ingestHandler — moqt.TrackHandler implementation
// ---------------------------------------------------------------------------

// ingestHandler bridges a single publish stream to MoQT subscribers.
// It implements [moqt.TrackHandler]; the TrackMux calls ServeTrack once per
// subscribing client.
//
// Track routing and catalog management are delegated to an [msf.Broadcast].
// Video and audio track handlers are registered via [registerVideo] and
// [registerAudio] when codec configuration becomes available.
type ingestHandler struct {
	broadcast *msf.Broadcast
	video     *videoTrack
	audio     *singleTrack
	once      sync.Once
}

func newIngestHandler(ctx context.Context) (*ingestHandler, error) {
	b, err := msf.NewBroadcast(msf.Catalog{Version: 1})
	if err != nil {
		return nil, fmt.Errorf("initializing broadcast: %w", err)
	}
	metricPublishersActive.Inc()
	return &ingestHandler{
		broadcast: b,
		video:     newVideoTrack(ctx),
		audio:     newSingleTrack(ctx),
	}, nil
}

// ServeTrack is called by TrackMux for each subscribing MoQT client. It
// blocks until the subscriber disconnects or the publisher ends.
func (h *ingestHandler) ServeTrack(tw *moqt.TrackWriter) {
	h.broadcast.ServeTrack(tw)
}

// TrackInfo implements moqt.TrackInfoProvider by returning the immutable publisher
// properties for video, audio, and the MSF catalog track according to moq-lite draft-05.
func (h *ingestHandler) TrackInfo(name moqt.TrackName) (moqt.PublishInfo, bool) {
	switch name {
	case "video":
		return moqt.PublishInfo{
			Priority:   128,
			Ordered:    true,
			MaxLatency: 2000,
			Timescale:  1_000_000,
		}, true
	case "audio":
		return moqt.PublishInfo{
			Priority:   128,
			Ordered:    false,
			MaxLatency: 2000,
			Timescale:  1_000_000,
		}, true
	case h.broadcast.CatalogTrackName():
		return moqt.PublishInfo{
			Priority:  255,
			Ordered:   true,
			Timescale: 1000,
		}, true
	default:
		return moqt.PublishInfo{}, false
	}
}

// registerVideo adds (or replaces) the video track in the broadcast catalog
// and wires a handler that streams from the video [trackBuffer].
func (h *ingestHandler) registerVideo(cfg *AVCConfig) error {
	if cfg == nil {
		return fmt.Errorf("video config must not be nil")
	}
	track := msf.Track{
		Name:      "video",
		Packaging: msf.PackagingLOC,
		Role:      msf.RoleVideo,
		IsLive:    new(true),
		Codec:     cfg.CodecString(),
		Width:     new(int64(cfg.Width)),
		Height:    new(int64(cfg.Height)),
	}
	if err := h.setInitData(&track, "video", videoInitData(cfg)); err != nil {
		return err
	}
	return h.broadcast.RegisterTrack(track, moqt.TrackHandlerFunc(func(tw *moqt.TrackWriter) {
		h.video.serve(tw)
	}))
}

// setInitData records data as an inline entry in the broadcast's catalog
// InitDataList and points track.InitRef at it (draft-ietf-moq-msf-01
// §5.1.7/§5.2.13 replaced the per-track inline initData with a catalog-level
// list referenced by id). A no-op when data is empty.
func (h *ingestHandler) setInitData(track *msf.Track, id, data string) error {
	if data == "" {
		return nil
	}
	catalog := h.broadcast.Catalog()
	replaced := false
	for i := range catalog.InitDataList {
		if catalog.InitDataList[i].ID == id {
			catalog.InitDataList[i].Data = data
			replaced = true
			break
		}
	}
	if !replaced {
		catalog.InitDataList = append(catalog.InitDataList, msf.InitDataRef{
			ID:   id,
			Type: "inline",
			Data: data,
		})
	}
	if err := h.broadcast.SetCatalog(catalog); err != nil {
		return fmt.Errorf("setting init data: %w", err)
	}
	track.InitRef = id
	return nil
}

// videoInitData returns the Base64-encoded AVCDecoderConfigurationRecord for a
// video catalog track — the codec init blob a browser WebCodecs VideoDecoder
// consumes as its `description`. Empty (with a warning) if it cannot be built.
func videoInitData(cfg *AVCConfig) string {
	rec, err := BuildAVCDecoderConfigurationRecord(cfg)
	if err != nil {
		slog.Warn("failed to build AVCDecoderConfigurationRecord for initData", "error", err)
		return ""
	}
	return base64.StdEncoding.EncodeToString(rec)
}

// registerAudio adds (or replaces) the audio track in the broadcast catalog
// and wires a handler that streams from the audio [trackBuffer].
func (h *ingestHandler) registerAudio(cfg *AACConfig) error {
	if cfg == nil {
		return fmt.Errorf("audio config must not be nil")
	}
	track := msf.Track{
		Name:          "audio",
		Packaging:     msf.PackagingLOC,
		Role:          msf.RoleAudio,
		IsLive:        new(true),
		Codec:         cfg.CodecString(),
		SampleRate:    new(int64(cfg.SampleRate)),
		ChannelConfig: strconv.Itoa(cfg.ChannelConfig),
	}
	if err := h.setInitData(&track, "audio", audioInitData(cfg)); err != nil {
		return err
	}
	return h.broadcast.RegisterTrack(track, moqt.TrackHandlerFunc(func(tw *moqt.TrackWriter) {
		h.audio.serve(tw)
	}))
}

// audioInitData returns the Base64-encoded AudioSpecificConfig for an audio
// catalog track — the codec init blob a browser WebCodecs AudioDecoder consumes
// as its `description`. Empty (with a warning) if it cannot be built.
func audioInitData(cfg *AACConfig) string {
	asc, err := BuildAudioSpecificConfig(cfg)
	if err != nil {
		slog.Warn("failed to build AudioSpecificConfig for initData", "error", err)
		return ""
	}
	return base64.StdEncoding.EncodeToString(asc)
}

// close signals all subscribers that the publisher has disconnected.
func (h *ingestHandler) close() {
	h.once.Do(func() {
		h.video.close()
		metricPublishersActive.Dec()
	})
}

// ---------------------------------------------------------------------------
// videoTrack — keyframe-based group management
// ---------------------------------------------------------------------------

// videoTrack manages a video track with keyframe-based group boundaries.
// Each keyframe opens a new MoQT group so that subscribers joining mid-stream
// always get a clean decode point (GOP alignment).
//
// A new group is opened on a keyframe only when its timestamp differs from the
// group currently being filled. This collapses redundant same-timestamp keyframe
// NALUs into one group: some publishers (e.g. ffmpeg's RTSP muxer in #229) emit
// several IDR NALUs back-to-back at the same presentation time, and opening a
// fresh group for each produces rapid micro-group churn that the relay ring /
// bounded collector window can deliver out of order — observed downstream as a
// spurious PTS regression. Logically those IDRs belong to one access unit, so
// one group is correct.
type videoTrack struct {
	buf       *trackBuffer
	ctx       context.Context
	currentMu sync.Mutex
	current   *sourceGroup
	currentTS int64 // presentation time (µs) of the group being filled; 0 before the first
}

func newVideoTrack(ctx context.Context) *videoTrack {
	return &videoTrack{buf: newTrackBuffer(ctx, "video"), ctx: ctx}
}

func (v *videoTrack) push(f *moqt.Frame, isKeyframe bool, timestampUS int64) {
	v.currentMu.Lock()
	if v.current == nil || (isKeyframe && timestampUS != v.currentTS) {
		if v.current != nil {
			v.current.complete.Store(true)
		}
		v.current = v.buf.openGroup()
		v.currentTS = timestampUS
	}
	v.current.append(f)
	v.currentMu.Unlock()

	v.buf.broadcast()
}

// serve writes buffered groups to a single MoQT TrackWriter. It blocks
// until the subscriber disconnects or the publisher ends.
func (v *videoTrack) serve(tw *moqt.TrackWriter) {
	v.buf.serve(v.ctx, tw)
}

// close marks the current open group as complete. Called when the publisher
// disconnects.
func (v *videoTrack) close() {
	v.currentMu.Lock()
	if v.current != nil {
		v.current.complete.Store(true)
	}
	v.currentMu.Unlock()

	v.buf.broadcast()
}

// ---------------------------------------------------------------------------
// singleTrack — one frame per group
// ---------------------------------------------------------------------------

// singleTrack manages a track where each push creates an independent,
// single-frame, immediately-complete group. This is the right model for
// audio (each AAC frame is independently decodable and warrants its own
// QUIC stream) and for catalog updates.
type singleTrack struct {
	buf *trackBuffer
	ctx context.Context
}

func newSingleTrack(ctx context.Context) *singleTrack {
	return &singleTrack{buf: newTrackBuffer(ctx, "audio"), ctx: ctx}
}

func (s *singleTrack) push(f *moqt.Frame) {
	g := s.buf.openGroup()
	g.append(f)
	g.complete.Store(true)

	s.buf.broadcast()
}

// pushFrames emits multiple frames as a single MoQT group, in slice order. Use
// when one source packet carries several access units (e.g. a multi-AU AAC RTP
// packet): one group keeps the frames on a single QUIC stream so the subscriber
// receives them in order, where pushing each as its own group would burst
// concurrent streams that race and arrive out of PTS order.
func (s *singleTrack) pushFrames(frames []*moqt.Frame) {
	if len(frames) == 0 {
		return
	}
	g := s.buf.openGroup()
	for _, f := range frames {
		g.append(f)
	}
	g.complete.Store(true)

	s.buf.broadcast()
}

// serve writes buffered groups to a single MoQT TrackWriter. It blocks
// until the subscriber disconnects or the publisher ends.
func (s *singleTrack) serve(tw *moqt.TrackWriter) {
	s.buf.serve(s.ctx, tw)
}

// ---------------------------------------------------------------------------
// trackBuffer — ring buffer + subscriber fan-out
// ---------------------------------------------------------------------------

// trackBuffer is the shared ring buffer with subscriber fan-out for a
// single MoQT track. Like [bytes.Buffer], it is a reusable data structure
// that decouples producers (push side) from consumers (serve side).
//
// Fan-out notification is the relay's O(1) broadcastNotify: an atomic
// sequence number plus a close-and-recreate channel. notify() used to walk
// every subscriber channel under an RWMutex on every pushed frame — O(N)
// per frame on the ingest critical path — and each subscriber cost a channel
// and two registry ops. Now a push advances a sequence number and closes one
// channel; egress compares sequences and parks on the current channel.
type trackBuffer struct {
	name string
	ctx  context.Context
	ring []atomic.Pointer[sourceGroup]
	size uint64
	pos  atomic.Uint64 // monotonically increasing; first group = 1

	notify broadcastNotify
}

func newTrackBuffer(ctx context.Context, name string) *trackBuffer {
	b := &trackBuffer{
		name: name,
		ctx:  ctx,
		ring: make([]atomic.Pointer[sourceGroup], defaultRingSize),
		size: defaultRingSize,
	}
	b.notify.init()
	go b.pollCacheDepth()
	return b
}

func (b *trackBuffer) pollCacheDepth() {
	defer metricBufferDepthGroups.DeleteLabelValues(b.name)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
			head := b.head()
			earliest := b.earliestAvailable()
			depth := 0
			if head >= earliest {
				depth = int(head - earliest + 1)
			}
			metricBufferDepthGroups.WithLabelValues(b.name).Set(float64(depth))
		}
	}
}

func (b *trackBuffer) openGroup() *sourceGroup {
	p := b.pos.Add(1)
	g := &sourceGroup{
		seq:    moqt.GroupSequence(p),
		frames: make([]*moqt.Frame, 0, 4),
	}
	b.ring[p%b.size].Store(g)
	return g
}

// --- subscriber notification ---

// broadcast advances the broadcast sequence, closing the previous notification
// channel — O(1): no per-subscriber walk, no registry lock on the push path.
// Egress goroutines see it as a sequence bump (waiters on the old channel are
// woken by the close). The former per-subscriber channel send loop ran this
// cost up on every pushed frame; per-frame fan-out measurements now live in
// BenchmarkTrackBufferNotify_Fanout.
func (b *trackBuffer) broadcast() {
	b.notify.notify()
}

// --- ring buffer access ---

func (b *trackBuffer) head() moqt.GroupSequence {
	return moqt.GroupSequence(b.pos.Load())
}

func (b *trackBuffer) earliestAvailable() moqt.GroupSequence {
	h := b.head()
	if h <= moqt.GroupSequence(b.size) {
		return 1
	}
	return h - moqt.GroupSequence(b.size) + 1
}

func (b *trackBuffer) get(seq moqt.GroupSequence) *sourceGroup {
	return b.ring[uint64(seq)%b.size].Load()
}

// --- subscriber egress ---

// serve writes groups and frames to a single MoQT TrackWriter. It blocks
// until the subscriber disconnects or ctx is cancelled.
func (b *trackBuffer) serve(ctx context.Context, tw *moqt.TrackWriter) {
	twCtx := tw.Context()

	metricSubscribersActive.Inc()
	defer metricSubscribersActive.Dec()

	// No per-subscriber registration: notification is a broadcast. Each egress
	// goroutine tracks the notify sequence it has consumed and compares it to
	// the current one; the channel is only for parking until the next advance.
	lastState := b.notify.listen()

	last := b.head()
	if last > 0 {
		last--
	}

	timer := time.NewTimer(notifyTimeout)
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	defer timer.Stop()

	// waitForNext parks until the notify sequence advances past seq, a timer
	// fires (the intra-group trickle fallback), or the session ends. Returns
	// true when the sequence advanced (or data may exist); false when ctx ended.
	// Mirrors the seq-guarded wait in the relay's deliverGroup: the seq check
	// catches a notify that already happened, selecting on the captured channel
	// catches one that happens after — so no wakeup is lost between checking
	// the head and parking.
	wait := func(seq uint64) bool {
		state := b.notify.listen()
		if state.seq > seq {
			return true
		}
		timer.Reset(notifyTimeout)
		select {
		case <-state.ch:
			return true
		case <-timer.C:
			return true
		case <-ctx.Done():
			return false
		case <-twCtx.Done():
			return false
		}
	}

	for {
		latest := b.head()

		if last < latest {
			last++

			earliest := b.earliestAvailable()
			if last < earliest {
				// Subscriber fell behind; skip to latest.
				metricSubscriberSkipsTotal.Inc()
				last = latest - 1
				continue
			}

			g := b.get(last)
			if g == nil {
				last--
				continue
			}

			gw, err := tw.OpenGroupAt(ctx, g.seq)
			if err != nil {
				metricSubscribeErrorsTotal.WithLabelValues("open_group_failed").Inc()
				return
			}
			start := time.Now()

			frameIdx := 0
			for {
				f := g.next(frameIdx)
				if f != nil {
					if err := gw.WriteFrame(f); err != nil {
						_ = gw.Close()
						return
					}
					frameIdx++
					continue
				}

				if g.isComplete() {
					break
				}

				// Wait for more frames within this group (trickle path). The
				// seq guard covers notifies that fired since the last frame
				// read; the timer covers a producer that stays silent past
				// notifyTimeout.
				if !wait(lastState.seq) {
					_ = gw.Close()
					return
				}
				lastState = b.notify.listen()
			}

			_ = gw.Close()
			metricGroupDeliveryHistogram.WithLabelValues(b.name).Observe(time.Since(start).Seconds())
			continue
		}

		// No new groups yet; wait for data (the level-triggered wakeup).
		if !wait(lastState.seq) {
			return
		}
		lastState = b.notify.listen()
	}
}

// ---------------------------------------------------------------------------
// sourceGroup
// ---------------------------------------------------------------------------

// sourceGroup holds the frames of a single MoQT group.
type sourceGroup struct {
	seq      moqt.GroupSequence
	mu       sync.Mutex
	frames   []*moqt.Frame
	complete atomic.Bool
}

func (g *sourceGroup) append(f *moqt.Frame) {
	g.mu.Lock()
	g.frames = append(g.frames, f)
	g.mu.Unlock()
}

func (g *sourceGroup) next(index int) *moqt.Frame {
	g.mu.Lock()
	defer g.mu.Unlock()
	if index < 0 || index >= len(g.frames) {
		return nil
	}
	return g.frames[index]
}

func (g *sourceGroup) isComplete() bool {
	return g.complete.Load()
}
