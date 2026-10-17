package ingest

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/rtsp"
)

// RTSPConfig holds configuration for an [RTSPServer].
type RTSPConfig struct {
	// Addr is the TCP address to listen on (e.g., ":554").
	Addr string

	// TrackMux is the MoQT multiplexer where ingested streams are announced.
	TrackMux *moqt.TrackMux

	// Authorize, when set, decides whether each ANNOUNCE may publish, from
	// the ?jwt= its URL carries; a refused one is answered 401. The
	// connection is cut when the permission lapses. Nil accepts every
	// ANNOUNCE.
	Authorize PublishAuthorizer
}

// RTSPServer accepts RTSP push connections and bridges each stream to
// MoQT via an ingest [Session].
type RTSPServer struct {
	config RTSPConfig

	mu       sync.Mutex
	listener *rtsp.Listener

	connWg     sync.WaitGroup
	connCancel context.CancelFunc
}

// NewRTSPServer creates a new RTSP ingest server with the given config.
func NewRTSPServer(cfg RTSPConfig) *RTSPServer {
	return &RTSPServer{config: cfg}
}

// ListenAndServe starts the RTSP listener and blocks until ctx is
// cancelled or an unrecoverable error occurs.
func (s *RTSPServer) ListenAndServe(ctx context.Context) error {
	ln, err := rtsp.Listen("tcp", s.config.Addr)
	if err != nil {
		return err
	}

	connCtx, connCancel := context.WithCancel(ctx)

	s.mu.Lock()
	s.listener = ln
	s.connCancel = connCancel
	s.mu.Unlock()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
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
func (s *RTSPServer) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Shutdown gracefully stops the RTSP listener.
func (s *RTSPServer) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	ln := s.listener
	s.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}

	done := make(chan struct{})
	go func() {
		s.connWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *RTSPServer) handleConn(ctx context.Context, conn *rtsp.Conn) {
	defer conn.Close()

	var (
		sess   *Session
		tracks = make(map[uint8]*rtspTrack) // channel -> track
		sdp    *rtsp.SDP
	)

	defer func() {
		if sess != nil {
			sess.Close()
		}
	}()

	// A read blocked on the connection doesn't see ctx end; closing the
	// connection ends it. cut closes it when the publisher's permission
	// lapses.
	defer context.AfterFunc(ctx, func() {
		// not actionable: the connection is being torn down either way.
		_ = conn.Close()
	})()
	var cut *time.Timer
	defer func() {
		if cut != nil {
			cut.Stop()
		}
	}()

	for {
		req, frame, err := conn.ReadRequest()
		if err != nil {
			if err != io.EOF {
				slog.Warn("RTSP read error", "error", err)
			}
			return
		}

		if frame != nil {
			if track, ok := tracks[frame.Channel]; ok {
				track.handleFrame(frame)
			}
			continue
		}

		// Handle RTSP Request
		resp := rtsp.NewResponse(rtsp.StatusOK, req)
		if cseq := req.Header.Get("CSeq"); cseq != "" {
			resp.Header.Set("CSeq", cseq)
		}

		switch req.Method {
		case rtsp.MethodOptions:
			resp.Header.Set("Public", "DESCRIBE, SETUP, TEARDOWN, PLAY, PAUSE, ANNOUNCE, RECORD")

		case rtsp.MethodAnnounce:
			if req.Body == nil {
				resp.StatusCode = rtsp.StatusBadRequest
				break
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				resp.StatusCode = rtsp.StatusBadRequest
				break
			}
			sdp = rtsp.ParseSDP(string(body))
			path := moqt.BroadcastPath(req.URL.Path)
			if path == "" {
				path = "/live/stream"
			}
			if s.config.Authorize != nil {
				lapses, err := s.config.Authorize(req.URL.Query().Get("jwt"), path)
				if err != nil {
					slog.Warn("RTSP publish refused",
						"remote", conn.RemoteAddr(),
						"broadcast_path", path,
						"error", err,
					)
					resp.StatusCode = rtsp.StatusUnauthorized
					break
				}
				if cut != nil {
					cut.Stop()
					cut = nil
				}
				if !lapses.IsZero() {
					cut = time.AfterFunc(time.Until(lapses), func() {
						slog.Info("RTSP ingest cut: the publisher's credential lapsed",
							"remote", conn.RemoteAddr(),
							"broadcast_path", path,
						)
						// not actionable: the connection is being cut.
						_ = conn.Close()
					})
				}
			}
			if sess != nil {
				sess.Close()
			}
			sess, err = NewSession(s.config.TrackMux, path)
			if err != nil {
				slog.Error("failed to create ingest session", "error", err)
				resp.StatusCode = rtsp.StatusInternalServerError
			}

		case rtsp.MethodSetup:
			if sdp == nil || sess == nil {
				// SETUP before ANNOUNCE, or ANNOUNCE failed.
				resp.StatusCode = rtsp.StatusBadRequest
				break
			}
			transport := req.Header.Get("Transport")
			if !strings.Contains(transport, "interleaved") {
				resp.StatusCode = rtsp.StatusUnsupportedTransport
				break
			}
			// Parse interleaved channels. ffmpeg sends parameters in varying
			// order, e.g. "RTP/AVP/TCP;unicast;interleaved=0-1" — so locate
			// the interleaved= token rather than matching the whole header.
			rtpChan, _, ok := parseInterleavedChannels(transport)
			if !ok {
				resp.StatusCode = rtsp.StatusBadRequest
				break
			}

			// Find corresponding media in SDP. An empty Control would match every
			// SETUP URL (strings.Contains("","")), so skip it.
			var media *rtsp.SDPMedia
			for i := range sdp.Medias {
				m := &sdp.Medias[i]
				if m.Control != "" && (strings.Contains(req.URL.String(), m.Control) || m.Control == "*") {
					media = m
					break
				}
			}

			track := newRTSPTrackFromMedia(sess, media)

			tracks[rtpChan] = track
			resp.Header.Set("Transport", transport)
			resp.Header.Set("Session", "12345678")

		case rtsp.MethodRecord:
			slog.Info("RTSP recording started", "remote", conn.RemoteAddr())

		case rtsp.MethodTeardown:
			_ = conn.WriteResponse(resp)
			return

		default:
			// Unsupported method (e.g. DESCRIBE, PLAY) for a push-only ingest.
			resp.StatusCode = rtsp.StatusMethodNotAllowed
		}

		if err := conn.WriteResponse(resp); err != nil {
			return
		}
	}
}

type trackKind int

const (
	trackKindVideo trackKind = iota
	trackKindAudio
)

type rtspTrack struct {
	session *Session
	kind    trackKind
	avcCfg  *AVCConfig

	// aacDepack reassembles AAC access units from mpeg4-generic RTP payloads.
	aacDepack *aacDepacketizer

	// fuBuffer reassembles an in-flight H.264 FU-A NAL unit across fragments.
	fuBuffer []byte

	// Access-unit reassembly for H.264 (RFC 6184). NAL units sharing an RTP
	// timestamp belong to one access unit; they are accumulated in auNALUs and
	// flushed as a single AVCC sample (concatenated length-prefixed NALUs) when
	// the timestamp changes or the RTP marker bit marks the AU boundary.
	auNALUs [][]byte
	auTS    uint32
	auOpen  bool

	// PTS zero-basing. RTSP carries each track on its own RTP clock (per-SSRC
	// random offset), so raw timestamps are not comparable across the audio and
	// video tracks and start at an arbitrary multi-hour value. Subtracting the
	// first frame's PTS puts every track on a common epoch (its first frame);
	// because a publisher starts audio and video together at media t=0, the two
	// zero-based timelines then share that epoch and advance in lockstep — which
	// is what makes A/V sync possible once a downstream scheduler (e.g. the
	// browser AudioWorklet) consumes the timestamps. RTMP is already zero-based.
	ptsBase  int64
	ptsBased bool
}

// normalizePTS subtracts the track's first-frame PTS baseline (lazily captured),
// returning a zero-based presentation timestamp. Idempotent after the first call.
func (t *rtspTrack) normalizePTS(pts int64) int64 {
	if !t.ptsBased {
		t.ptsBase = pts
		t.ptsBased = true
	}
	return pts - t.ptsBase
}

func (t *rtspTrack) handleFrame(f *rtsp.InterleavedFrame) {
	packet, err := rtsp.UnmarshalRTP(f.Payload)
	if err != nil {
		return
	}

	if t.kind == trackKindVideo {
		t.handleVideoRTP(packet)
	} else {
		t.handleAudioRTP(packet)
	}
}

func (t *rtspTrack) handleAudioRTP(p *rtsp.RTPPacket) {
	if t.session == nil || t.aacDepack == nil {
		return
	}
	aus, err := t.aacDepack.depacketize(p.Payload, p.Header.Timestamp)
	if err != nil {
		return
	}
	// Coalesce every access unit carried by this RTP packet into a single
	// MoQT group. ffmpeg packs 3–4 AAC frames per mpeg4-generic packet; pushing
	// them as N separate groups bursts N concurrent QUIC streams (MoQT maps one
	// group to one stream), and gomoqt delivers groups in stream-arrival order
	// — so a burst arrives out of PTS order and the decoder pops. One group
	// keeps the frames on a single stream, delivered in order.
	ptss := make([]int64, len(aus))
	frames := make([][]byte, len(aus))
	for i, au := range aus {
		ptss[i] = t.normalizePTS(au.pts)
		frames[i] = au.data
	}
	t.session.PushAudioFrames(ptss, frames)
}

func (t *rtspTrack) handleVideoRTP(p *rtsp.RTPPacket) {
	if len(p.Payload) == 0 {
		return
	}

	// A change in RTP timestamp marks the start of a new access unit (RFC 6184
	// §7.1.1): flush the accumulated NALUs as one AVCC sample before admitting
	// any NALU from the new timestamp. This is also the safety net for the
	// marker-bit boundary below.
	if t.auOpen && p.Header.Timestamp != t.auTS {
		t.flushAccessUnit()
	}
	t.auTS = p.Header.Timestamp
	t.auOpen = true
	t.auNALUs = append(t.auNALUs, t.extractNALUs(p.Payload)...)

	// The RTP marker bit is set on the last packet of an access unit; flushing
	// here gives the lowest-latency boundary and avoids holding the AU until the
	// next timestamp arrives.
	if p.Header.Marker {
		t.flushAccessUnit()
	}
}

// extractNALUs returns the complete H.264 NAL units carried by a single RTP
// packet payload, handling the three RFC 6184 payload types:
//
//   - Single NAL (types 1–23): the whole payload is one NALU.
//   - STAP-A (type 24): several NALUs aggregated with 2-byte length prefixes.
//   - FU-A (type 28): a fragment; reassembled across packets, returned only on
//     the final fragment.
//
// NAL types 0 and 25–31 are reserved/ignored. Returns nil for an FU-A middle
// fragment (the NALU is still in flight).
func (t *rtspTrack) extractNALUs(payload []byte) [][]byte {
	typ := payload[0] & 0x1F
	switch {
	case typ >= 1 && typ <= 23:
		return [][]byte{payload}
	case typ == 24:
		return parseSTAPA(payload)
	case typ == 28:
		if nalu := t.reassembleFU(payload); nalu != nil {
			return [][]byte{nalu}
		}
		return nil
	}
	return nil
}

// parseSTAPA splits an H.264 STAP-A aggregation packet (RFC 6184 §5.7) into its
// component NAL units. Each entry is [uint16BE length][NALU]. A truncated entry
// stops parsing; the well-formed NALUs parsed so far are returned.
func parseSTAPA(payload []byte) [][]byte {
	var nalus [][]byte
	for off := 1; off+2 <= len(payload); {
		n := int(binary.BigEndian.Uint16(payload[off:]))
		off += 2
		if off+n > len(payload) || n == 0 {
			break
		}
		nalus = append(nalus, payload[off:off+n:off+n])
		off += n
	}
	return nalus
}

// flushAccessUnit emits the accumulated access unit as a single AVCC sample
// (concatenated 4-byte-length-prefixed NALUs) — the sample-stream format
// matching the avc1 codec string and catalog initData — and resets the
// accumulator. It is a no-op when no NALUs are accumulated.
//
// Pushing one AVCC sample per access unit (rather than one per NALU) is what
// the WebCodecs VideoDecoder expects: an EncodedVideoChunk is one access unit.
// ffmpeg's RTSP muxer emits several IDR NALUs at the same presentation
// timestamp within one access unit; emitting them as separate same-PTS frames
// caused two of every three to be marked `delta` by the player and decoded as
// competing pictures.
func (t *rtspTrack) flushAccessUnit() {
	if !t.auOpen || len(t.auNALUs) == 0 {
		t.auOpen = false
		t.auNALUs = nil
		return
	}
	var data []byte
	isKey := false
	for _, nalu := range t.auNALUs {
		data = appendAVCC(data, nalu)
		if nalu[0]&0x1F == 5 { // IDR slice
			isKey = true
		}
	}
	pts := t.normalizePTS(int64(t.auTS) * 1000 / 90) // 90 kHz clock → microseconds
	if t.session != nil {
		t.session.PushVideo(pts, data, isKey)
	}
	t.auNALUs = nil
	t.auOpen = false
}

// maxFUBufferSize caps a single reassembled H.264 NAL unit. A real NAL is far
// smaller (even a 4K intra frame is a few MB), so this purely bounds memory
// against a malformed or hostile RTSP publisher that streams FU-A continuation
// fragments without ever setting the end bit. A var (not const) so tests can
// shrink it.
var maxFUBufferSize = 16 << 20 // 16 MiB

// reassembleFU handles H.264 FU-A (RFC 6184 §5.8) fragmentation. Each
// fragment's payload is appended to fuBuffer; the completed NAL unit is
// returned when the final (end) fragment arrives, and nil while a NAL unit is
// still in flight. The reconstructed first byte combines the FU indicator's
// forbidden-zero-bit + NRI (top 3 bits, 0xE0) with the FU header's NAL type
// (low 5 bits). Callers push the returned NAL unit via [extractNALUs].
//
// Safety: a middle/end fragment with no active reassembly (no preceding start)
// is dropped rather than appended to a nil buffer, which would yield a NAL
// missing its reconstructed header byte. If fuBuffer exceeds maxFUBufferSize
// the in-flight NAL is discarded and reassembly resets, bounding memory.
func (t *rtspTrack) reassembleFU(payload []byte) []byte {
	if len(payload) < 2 {
		return nil
	}
	fuHeader := payload[1]
	start := (fuHeader >> 7) & 1
	end := (fuHeader >> 6) & 1
	nalType := fuHeader & 0x1F

	if start == 1 {
		t.fuBuffer = []byte{(payload[0] & 0xE0) | nalType}
	} else if t.fuBuffer == nil {
		// Middle/end fragment without an active reassembly: drop it instead of
		// building a headerless (malformed) NAL unit.
		return nil
	}
	t.fuBuffer = append(t.fuBuffer, payload[2:]...)

	if len(t.fuBuffer) > maxFUBufferSize {
		// Exceeds the cap: abandon this NAL and reset, bounding memory.
		t.fuBuffer = nil
		return nil
	}

	if end == 1 {
		complete := t.fuBuffer
		t.fuBuffer = nil
		return complete
	}
	return nil
}

// appendAVCC appends a NAL unit, prefixed with a 4-byte big-endian length, to
// data — the AVCC sample-stream format that matches the avc1 codec string and
// the AVCDecoderConfigurationRecord carried in the catalog initData.
func appendAVCC(data, nalu []byte) []byte {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(nalu)))
	data = append(data, lenBuf[:]...)
	data = append(data, nalu...)
	return data
}

// wrapAVCC prefixes a single NAL unit with a 4-byte big-endian length. Retained
// for tests that exercise the single-NAL framing directly.
func wrapAVCC(nalu []byte) []byte {
	data := make([]byte, 4+len(nalu))
	binary.BigEndian.PutUint32(data, uint32(len(nalu)))
	copy(data[4:], nalu)
	return data
}

// newRTSPTrackFromMedia builds an rtspTrack from an SDP media description and
// registers it with the session. Shared by the push SETUP handler (above) and
// the pull DESCRIBE path (rtsp_pull.go). Codec detection is case-insensitive
// (RFC 3555); H.264 video and mpeg4-generic/AAC audio are supported; anything
// else returns a track with kind=0 (uninitialised) which the caller should skip.
func newRTSPTrackFromMedia(sess *Session, media *rtsp.SDPMedia) *rtspTrack {
	track := &rtspTrack{session: sess}
	if media == nil {
		return track
	}
	rtpMap := strings.ToLower(media.RtpMap)
	if media.Type == "video" && strings.Contains(rtpMap, "h264") {
		track.kind = trackKindVideo
		if fmtp := media.Fmtp; fmtp != "" {
			sps, pps := extractParameterSets(fmtp)
			if len(sps) > 0 && len(sps[0]) >= 4 {
				cfg := &AVCConfig{
					ProfileIDC:    sps[0][1],
					ProfileCompat: sps[0][2],
					LevelIDC:      sps[0][3],
					NALULenSize:   4,
					SPS:           sps,
					PPS:           pps,
				}
				cfg.Width, cfg.Height = parseSPSDimensions(sps[0])
				if err := sess.RegisterVideo(cfg); err != nil {
					slog.Warn("failed to register video track", "error", err)
				}
				track.avcCfg = cfg
			}
		}
	} else if media.Type == "audio" && strings.Contains(rtpMap, "mpeg4-generic") {
		track.kind = trackKindAudio
		cfg := parseAACConfigFromFmtp(media.Fmtp)
		track.aacDepack = newAACDepacketizer(media.Fmtp, cfg.SampleRate)
		if err := sess.RegisterAudio(cfg); err != nil {
			slog.Warn("failed to register audio track", "error", err)
		}
	}
	return track
}

func extractParameterSets(fmtp string) (sps, pps [][]byte) {
	_, after, ok := strings.Cut(fmtp, "sprop-parameter-sets=")
	if !ok {
		return nil, nil
	}
	sets := strings.SplitSeq(strings.Split(after, ";")[0], ",")
	for s := range sets {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(b) == 0 {
			continue
		}
		switch b[0] & 0x1F {
		case 7:
			sps = append(sps, b)
		case 8:
			pps = append(pps, b)
		}
	}
	return sps, pps
}

// parseInterleavedChannels extracts the RTP/RTCP interleaved channel pair from
// an RTSP Transport header. RTSP clients (e.g. ffmpeg) send transport
// parameters in varying order — "RTP/AVP/TCP;unicast;interleaved=0-1" — so the
// interleaved= token is located rather than matching the whole header. A single
// channel form "interleaved=N" maps both RTP and RTCP to N.
func parseInterleavedChannels(transport string) (rtp, rtcp uint8, ok bool) {
	const token = "interleaved="
	_, after, ok := strings.Cut(transport, token)
	if !ok {
		return 0, 0, false
	}
	rest := after
	var a, b int
	if n, err := fmt.Sscanf(rest, "%d-%d", &a, &b); err == nil && n == 2 {
		if !validChannel(a) || !validChannel(b) {
			return 0, 0, false
		}
		return uint8(a), uint8(b), true
	}
	if n, err := fmt.Sscanf(rest, "%d", &a); err == nil && n == 1 {
		if !validChannel(a) {
			return 0, 0, false
		}
		return uint8(a), uint8(a), true
	}
	return 0, 0, false
}

// validChannel reports whether n fits in the 8-bit RTSP interleaved channel
// space (0–255).
func validChannel(n int) bool {
	return n >= 0 && n <= 255
}
