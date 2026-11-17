// Package ingest bridges media publish protocols (RTMP, SRT, WHIP, etc.)
// to MoQT. Each protocol adapter accepts incoming connections, extracts
// audio/video frames, and pushes them into a protocol-agnostic [Session].
// The Session announces the media on a [moqt.TrackMux] so that MoQT
// subscribers can consume the stream.
//
// # Adding a new protocol
//
// Implement a listener that accepts connections for the target protocol,
// create a [Session] per publish stream, register tracks when codec
// configuration becomes available, and push frames:
//
//	sess, err := ingest.NewSession(trackMux, "/app/stream")
//	if err != nil { /* handle */ }
//	defer sess.Close()
//	sess.RegisterVideo(avcCfg)
//	sess.RegisterAudio(aacCfg)
//	for {
//	    ts, data, isKey := readFromProtocol()
//	    sess.PushVideo(ts, data, isKey)
//	}
package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/qumo-dev/qumo/internal/optin"
	"github.com/qumo-dev/qumo/token"
)

// Session manages the MoQT announcement and fan-out for a single ingested
// publish stream, regardless of the source protocol. Protocol adapters
// create a Session via [NewSession] and push media data through
// [Session.PushVideo] and [Session.PushAudio].
//
// A Session is safe for concurrent use; however, callers should typically
// push video and audio from a single goroutine per track.
type Session struct {
	path     moqt.BroadcastPath
	trackMux *moqt.TrackMux
	handler  *ingestHandler
	endAnn   func()
	// ctx ends when the session closes, and with it every opt-in.
	ctx    context.Context
	cancel context.CancelFunc
}

// NewSession creates a new ingest session that announces path on trackMux.
// Subscribers that request tracks under this broadcast path will receive
// the media pushed via [Session.PushVideo] and [Session.PushAudio].
//
// Call [Session.Close] when the publisher disconnects.
func NewSession(trackMux *moqt.TrackMux, path moqt.BroadcastPath) (*Session, error) {
	if !path.HasPrefix("/") {
		return nil, fmt.Errorf("ingest session: invalid broadcast path %q (must start with '/')", path)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ann, endAnn := moqt.NewAnnouncement(ctx, path)

	h, err := newIngestHandler(ctx)
	if err != nil {
		endAnn()
		cancel()
		return nil, fmt.Errorf("ingest session %s: %w", path, err)
	}
	trackMux.Announce(ann, h)

	slog.Info("ingest session started", "broadcast_path", path)

	return &Session{
		path:     path,
		trackMux: trackMux,
		handler:  h,
		endAnn:   endAnn,
		ctx:      ctx,
		cancel:   cancel,
	}, nil
}

// OptIn opts the session's broadcast into the egress o names, as a MoQT
// publisher does (see package optin): it announces the broadcast's path
// beneath o's directory, redirecting every subscription to the broadcast, on
// the session's TrackMux. The opt-in is withdrawn at lapses, or when the
// session closes; the zero time never lapses.
func (s *Session) OptIn(o token.OptIn, lapses time.Time) {
	ctx, cancel := s.ctx, context.CancelFunc(func() {})
	if !lapses.IsZero() {
		ctx, cancel = context.WithDeadline(s.ctx, lapses)
	}
	path := optin.Path(o.Directory(), s.path)
	// The announcement ends with ctx.
	ann, _ := moqt.NewAnnouncement(ctx, path)
	context.AfterFunc(ctx, func() {
		cancel()
		slog.Info("ingest opt-in withdrawn", "broadcast_path", s.path, "opt_in", o)
	})
	s.trackMux.Announce(ann, optin.Handler(s.path))
	slog.Info("ingest opted in", "broadcast_path", s.path, "opt_in", o, "opt_in_path", path)
}

// RegisterVideo registers (or replaces) the video track in the MSF catalog
// using codec configuration extracted from a sequence header. Subscribers
// can discover the track via the catalog and receive video frames pushed
// through [Session.PushVideo].
func (s *Session) RegisterVideo(cfg *AVCConfig) error {
	return s.handler.registerVideo(cfg)
}

// RegisterAudio registers (or replaces) the audio track in the MSF catalog
// using codec configuration extracted from a sequence header. Subscribers
// can discover the track via the catalog and receive audio frames pushed
// through [Session.PushAudio].
func (s *Session) RegisterAudio(cfg *AACConfig) error {
	return s.handler.registerAudio(cfg)
}

// PushVideo appends video data (codec-specific: AVCC length-prefixed NALUs
// for both RTMP and RTSP ingest) as a MoQT frame wrapped in a MediaFrame
// envelope. When isKeyframe is true a new MoQT group is opened (GOP boundary).
//
// timestampUS is the presentation timestamp in microseconds.
func (s *Session) PushVideo(timestampUS int64, data []byte, isKeyframe bool) {
	f := moqt.NewFrame(mediaFrameSize(timestampUS, len(data)))
	writeMediaFrame(f, timestampUS, data)
	s.handler.video.push(f, isKeyframe, timestampUS)
}

// PushAudio appends converted audio data (raw AAC frame) as an
// independently-decodable MoQT group wrapped in a MediaFrame envelope.
//
// timestampUS is the presentation timestamp in microseconds.
func (s *Session) PushAudio(timestampUS int64, data []byte) {
	f := moqt.NewFrame(mediaFrameSize(timestampUS, len(data)))
	writeMediaFrame(f, timestampUS, data)
	s.handler.audio.push(f)
}

// PushAudioFrames appends one or more raw AAC frames as a single MoQT group,
// each wrapped in a MediaFrame envelope and ordered by its presentation
// timestamp. Use this when a source packet carries several access units (e.g.
// an mpeg4-generic RTP packet packing 3–4 AAC frames): coalescing them into one
// group avoids a burst of concurrent MoQT groups whose QUIC streams can be
// delivered out of order, which would reach the decoder non-monotonically and
// pop. The single-frame [PushAudio] is the degenerate case.
//
// Each timestampsUS[i] is the presentation timestamp, in microseconds, of
// frames[i]. The slices must have equal length; a nil/empty call is a no-op.
func (s *Session) PushAudioFrames(timestampsUS []int64, frames [][]byte) {
	if len(frames) == 0 || len(timestampsUS) != len(frames) {
		return
	}
	built := make([]*moqt.Frame, len(frames))
	for i, data := range frames {
		pts := timestampsUS[i]
		f := moqt.NewFrame(mediaFrameSize(pts, len(data)))
		writeMediaFrame(f, pts, data)
		built[i] = f
	}
	s.handler.audio.pushFrames(built)
}

// Close ends the MoQT announcement and signals all subscribers that
// the publisher has disconnected. It is safe to call multiple times.
func (s *Session) Close() {
	s.handler.close()
	s.endAnn()
	s.cancel()
	slog.Info("ingest session ended", "broadcast_path", s.path)
}
