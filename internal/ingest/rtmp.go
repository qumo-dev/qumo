package ingest

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/rtmp"
)

// RTMPConfig holds configuration for an [RTMPServer].
type RTMPConfig struct {
	// Addr is the TCP address to listen on (e.g., ":1935").
	Addr string

	// TrackMux is the MoQT multiplexer where ingested streams are announced.
	TrackMux *moqt.TrackMux

	// Authorize, when set, decides whether each publish may go ahead, from
	// the ?jwt= its stream key (or app) carries; a refused one is answered
	// NetStream.Publish.Denied. The stream is cut when the permission
	// lapses. Nil accepts every publish.
	Authorize PublishAuthorizer
}

// RTMPServer accepts RTMP publish connections and bridges each stream to
// MoQT via an ingest [Session].
type RTMPServer struct {
	config RTMPConfig

	mu       sync.Mutex
	listener *rtmp.Listener

	// Active connection tracking for graceful shutdown.
	connWg     sync.WaitGroup
	connCancel context.CancelFunc // cancels all active connection contexts
}

// NewRTMPServer creates a new RTMP ingest server with the given config.
func NewRTMPServer(cfg RTMPConfig) *RTMPServer {
	return &RTMPServer{config: cfg}
}

// ListenAndServe starts the RTMP listener and blocks until ctx is
// cancelled or an unrecoverable error occurs. Each accepted connection
// is handled in a separate goroutine.
func (s *RTMPServer) ListenAndServe(ctx context.Context) error {
	ln, err := rtmp.Listen("tcp", s.config.Addr)
	if err != nil {
		return err
	}

	connCtx, connCancel := context.WithCancel(ctx)

	s.mu.Lock()
	s.listener = ln
	s.connCancel = connCancel
	s.mu.Unlock()

	// Close the listener when ctx is cancelled so Accept unblocks.
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			// Expected after Close/Shutdown.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		s.connWg.Go(func() {
			s.handleConn(connCtx, conn)
		})
	}
}

// Addr returns the address the server is listening on, or nil before
// ListenAndServe has bound the listener. Lets a caller that configured
// Addr ":0" learn the actual bound address.
func (s *RTMPServer) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Close stops the RTMP listener and cancels all active connections
// immediately.
func (s *RTMPServer) Close() error {
	s.mu.Lock()
	cancel := s.connCancel
	ln := s.listener
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if ln != nil {
		return ln.Close()
	}
	return nil
}

// Shutdown gracefully stops the RTMP listener so no new connections are
// accepted, then waits for active connections to finish or ctx to expire.
func (s *RTMPServer) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	ln := s.listener
	s.mu.Unlock()

	// Stop accepting new connections.
	if ln != nil {
		_ = ln.Close()
	}

	// Wait for active connections or ctx cancellation.
	done := make(chan struct{})
	go func() {
		s.connWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// Force-close remaining connections.
		_ = s.Close()
		return ctx.Err()
	}
}

func (s *RTMPServer) handleConn(ctx context.Context, conn *rtmp.Conn) {
	defer conn.Close()

	var (
		path   moqt.BroadcastPath
		lapses time.Time
	)
	mr, err := conn.AcceptStreamFunc(func(app, streamKey string) error {
		var credential string
		path, credential = rtmpTarget(app, streamKey)
		if s.config.Authorize == nil {
			return nil
		}
		var err error
		lapses, err = s.config.Authorize(credential, path)
		return err
	})
	if err != nil {
		// The path, never the stream key: that may carry the credential.
		slog.Warn("failed to accept RTMP stream",
			"remote", conn.RemoteAddr(),
			"broadcast_path", path,
			"error", err,
		)
		return
	}
	defer mr.Close()

	ctx, cancel := untilLapse(ctx, lapses)
	defer cancel()
	// A read blocked on the connection doesn't see ctx end; closing the
	// connection ends it.
	defer context.AfterFunc(ctx, func() {
		// not actionable: the connection is being torn down either way.
		_ = conn.Close()
	})()

	sess, err := NewSession(s.config.TrackMux, path)
	if err != nil {
		slog.Warn("failed to create ingest session",
			"remote", conn.RemoteAddr(),
			"broadcast_path", path,
			"error", err,
		)
		return
	}
	defer sess.Close()

	slog.Info("RTMP ingest started",
		"remote", conn.RemoteAddr(),
		"broadcast_path", path,
	)
	slog.Info("subscribe info",
		"broadcast_path", path,
		"tracks", []string{"catalog", "video", "audio"},
	)
	defer slog.Info("RTMP ingest ended",
		"remote", conn.RemoteAddr(),
		"broadcast_path", path,
	)

	ingestRTMP(ctx, mr, sess)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		slog.Info("RTMP ingest cut: the publisher's credential lapsed",
			"remote", conn.RemoteAddr(),
			"broadcast_path", path,
		)
	}
}

// ingestRTMP reads frames from an RTMP MessageReader and pushes them
// into a Session. It blocks until the reader returns an error, the
// publisher disconnects, or ctx is cancelled.
//
// FLV video/audio sequence headers are parsed to extract codec
// configuration. Subsequent video NALUs are forwarded unchanged as AVCC
// (length-prefixed) inside MediaFrame envelopes; SPS/PPS are carried in
// the catalog initData. AAC raw frames have the FLV header stripped.
func ingestRTMP(ctx context.Context, mr *rtmp.MessageReader, sess *Session) {
	var (
		avcCfg *AVCConfig
		aacCfg *AACConfig
	)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		frame, err := mr.ReadFrame()
		if err != nil {
			return
		}

		switch frame.Type {
		case rtmp.FrameTypeVideo:
			if IsVideoSequenceHeader(frame.Data) {
				cfg, err := ParseAVCConfig(frame.Data)
				if err != nil {
					slog.Warn("failed to parse AVC config", "error", err)
					continue
				}
				avcCfg = cfg
				slog.Info("AVC config received",
					"codec", cfg.CodecString(),
					"width", cfg.Width,
					"height", cfg.Height,
				)
				if err := sess.RegisterVideo(avcCfg); err != nil {
					slog.Warn("failed to register video track", "error", err)
				}
				continue
			}

			if avcCfg == nil {
				// Drop video frames until we have a sequence header.
				continue
			}

			if len(frame.Data) < 5 {
				slog.Debug("video NALU tag too short")
				continue
			}

			// Forward AVCC NALUs unchanged (bytes 5+). The browser's
			// VideoDecoder consumes AVCC directly; SPS/PPS are carried in the
			// catalog initData (built from the sequence header). PTS = DTS +
			// CTS preserves B-frame composition timing.
			cts := parseFLVVideoCTS(frame.Data)
			pts := (int64(frame.Timestamp) + int64(cts)) * 1000
			sess.PushVideo(pts, frame.Data[5:], isVideoKeyframe(frame.Data))

		case rtmp.FrameTypeAudio:
			if IsAudioSequenceHeader(frame.Data) {
				cfg, err := ParseAACConfig(frame.Data)
				if err != nil {
					slog.Warn("failed to parse AAC config", "error", err)
					continue
				}
				aacCfg = cfg
				slog.Info("AAC config received",
					"codec", cfg.CodecString(),
					"sample_rate", cfg.SampleRate,
					"channels", cfg.ChannelConfig,
				)
				if err := sess.RegisterAudio(aacCfg); err != nil {
					slog.Warn("failed to register audio track", "error", err)
				}
				continue
			}

			raw, err := StripFLVAudioHeader(frame.Data)
			if err != nil {
				slog.Debug("failed to strip FLV audio header", "error", err)
				continue
			}

			pts := int64(frame.Timestamp) * 1000
			sess.PushAudio(pts, raw)
		}
	}
}

// isVideoKeyframe reports whether the raw FLV/RTMP video tag data begins
// with a keyframe indicator. It handles both standard RTMP (FrameType in
// the upper nibble) and Enhanced RTMP (FrameType in bits 4–6).
func isVideoKeyframe(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	// Standard: byte 0 = FrameType(4) | CodecID(4); keyframe = 1.
	// Enhanced: byte 0 = isExHeader(1) | FrameType(3) | PacketType(4); keyframe = 1.
	// Masking bits 4–6 works for both layouts.
	return (data[0]>>4)&0x07 == 1
}
