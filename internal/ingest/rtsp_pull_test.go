package ingest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/rtsp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveControlURL(t *testing.T) {
	const session = "rtsp://camera:554/stream"

	tests := map[string]struct {
		control string
		want    string
	}{
		"empty → session URL":    {"", session},
		"star → session URL":     {"*", session},
		"absolute rtsp URL":      {"rtsp://camera:554/stream/trackID=1", "rtsp://camera:554/stream/trackID=1"},
		"relative trackID":       {"trackID=0", "rtsp://camera:554/stream/trackID=0"},
		"relative path":          {"/stream/video", "rtsp://camera:554/stream/video"},
		"relative without slash": {"video", "rtsp://camera:554/stream/video"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := resolveControlURL(tc.control, session)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestRedactURL(t *testing.T) {
	assert.Equal(t, "rtsp://camera:554/stream", redactURL("rtsp://admin:secret@camera:554/stream"))
	assert.Equal(t, "rtsp://camera:554/stream", redactURL("rtsp://camera:554/stream"))
	assert.Equal(t, "garbage", redactURL("garbage")) // unparseable → returned as-is
}

func TestSameOrigin(t *testing.T) {
	const session = "rtsp://camera:554/stream"
	assert.True(t, sameOrigin("rtsp://camera:554/stream/trackID=0", session)) // same origin
	assert.True(t, sameOrigin("rtsp://camera:554/other", session))            // same host:port
	assert.False(t, sameOrigin("rtsp://evil.com/stream", session))            // different host (SSRF)
	assert.False(t, sameOrigin("http://camera:554/stream", session))          // different scheme
	assert.False(t, sameOrigin("rtsp://camera:555/stream", session))          // different port
}

// --- Integration tests: fake RTSP server + pull client → Session ---

// buildRTPHeader constructs a minimal 12-byte RTP header.
func buildRTPHeader(pt uint8, marker bool, seq uint16, ts, ssrc uint32) []byte {
	h := make([]byte, 12)
	h[0] = 0x80 // V=2, no padding/ext/csrc
	h[1] = pt
	if marker {
		h[1] |= 0x80
	}
	binary.BigEndian.PutUint16(h[2:4], seq)
	binary.BigEndian.PutUint32(h[4:8], ts)
	binary.BigEndian.PutUint32(h[8:12], ssrc)
	return h
}

// buildTestSDP constructs an SDP with H.264 video (and optionally AAC audio).
func buildTestSDP(includeAudio bool) string {
	sps := []byte{0x67, 0x64, 0x00, 0x1F, 0xAC, 0xD9} // NAL type 7
	pps := []byte{0x68, 0xEB, 0xE3, 0xCB}             // NAL type 8
	sprop := base64.StdEncoding.EncodeToString(sps) + "," + base64.StdEncoding.EncodeToString(pps)
	lines := []string{
		"v=0",
		"m=video 0 RTP/AVP 96",
		"a=rtpmap:96 H264/90000",
		"a=fmtp:96 packetization-mode=1; sprop-parameter-sets=" + sprop + "; profile-level-id=64001f",
		"a=control:trackID=0",
	}
	if includeAudio {
		lines = append(lines,
			"m=audio 0 RTP/AVP 97",
			"a=rtpmap:97 mpeg4-generic/48000/2",
			"a=fmtp:97 streamtype=5; profile-level-id=1; mode=AAC-hbr; sizelength=13; indexlength=3; indexdeltalength=3; config=1190",
			"a=control:trackID=1",
		)
	}
	return strings.Join(append(lines, ""), "\r\n")
}

// buildTestVideoRTP returns a single-NAL IDR H.264 RTP packet (PT=96, marker=1).
func buildTestVideoRTP() []byte {
	return append(buildRTPHeader(96, true, 1, 0, 0x12345678),
		0x65, 0xAA, 0xBB, 0xCC, 0xDD, // IDR NAL (NRI=3, type=5)
	)
}

// buildTestAudioRTP returns a mpeg4-generic AAC RTP packet (PT=97, marker=1).
func buildTestAudioRTP() []byte {
	aacAU := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	payload := buildMpeg4Generic([][]byte{aacAU}, 13, 3)
	return append(buildRTPHeader(97, true, 1, 0, 0x87654321), payload...)
}

// startFakeRTSPServer starts a minimal RTSP server that responds to
// OPTIONS/DESCRIBE/SETUP/PLAY and streams interleaved RTP (in order) before
// closing. If authScheme is non-empty ("basic" or "digest"), DESCRIBE returns
// 401 on the first attempt (without Authorization) and 200 on the retry.
func startFakeRTSPServer(t *testing.T, sdpBody string, frames []rtsp.InterleavedFrame, authScheme string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		rc := rtsp.NewConn(conn)
		for {
			req, _, err := rc.ReadRequest()
			if err != nil {
				return
			}
			resp := rtsp.NewResponse(rtsp.StatusOK, req)
			if cseq := req.Header.Get("CSeq"); cseq != "" {
				resp.Header.Set("CSeq", cseq)
			}
			switch req.Method {
			case rtsp.MethodOptions:
				resp.Header.Set("Public", "DESCRIBE, SETUP, PLAY, TEARDOWN")
			case rtsp.MethodDescribe:
				if authScheme != "" && req.Header.Get("Authorization") == "" {
					// Challenge the first unauthenticated DESCRIBE.
					resp.StatusCode = rtsp.StatusUnauthorized
					switch authScheme {
					case "basic":
						resp.Header.Set("WWW-Authenticate", `Basic realm="test"`)
					case "digest":
						resp.Header.Set("WWW-Authenticate",
							`Digest realm="test", nonce="abc123", qop="auth", algorithm=MD5`)
					}
				} else {
					body := []byte(sdpBody)
					resp.Header.Set("Content-Type", "application/sdp")
					resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
					resp.Body = io.NopCloser(bytes.NewReader(body))
				}
			case rtsp.MethodSetup:
				resp.Header.Set("Transport", req.Header.Get("Transport"))
				resp.Header.Set("Session", "fake-session")
			case rtsp.MethodPlay:
				_ = rc.WriteResponse(resp)
				for _, f := range frames {
					_ = rc.WriteInterleavedFrame(&f)
				}
				return
			}
			_ = rc.WriteResponse(resp)
		}
	}()

	return ln.Addr().String()
}

// runPullAndVerify runs pullStream against addr and asserts both expected
// buffers received data. videoExpected/audioExpected control which to check.
func runPullAndVerify(t *testing.T, addr, url string, sess *Session, videoExpected, audioExpected bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- pullStream(ctx, url, sess) }()

	require.Eventually(t, func() bool {
		ok := true
		if videoExpected {
			ok = ok && sess.handler.video.buf.head() > 0
		}
		if audioExpected {
			ok = ok && sess.handler.audio.buf.head() > 0
		}
		return ok
	}, 3*time.Second, 50*time.Millisecond, "expected frames did not arrive in Session")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pullStream did not return after cancel")
	}
}

// TestPullStream_DefaultCase exercises the full pull pipeline: a fake RTSP
// server with H.264 video + AAC audio → the pull client (DESCRIBE/SETUP/PLAY)
// → interleaved RTP → depacketize → Session buffers receive frames.
func TestPullStream_DefaultCase(t *testing.T) {
	addr := startFakeRTSPServer(t, buildTestSDP(true), []rtsp.InterleavedFrame{
		{Channel: 0, Payload: buildTestVideoRTP()},
		{Channel: 2, Payload: buildTestAudioRTP()},
	}, "")

	trackMux := moqt.NewTrackMux(0)
	sess, err := NewSession(trackMux, "/live/camera")
	require.NoError(t, err)
	defer sess.Close()

	runPullAndVerify(t, addr, "rtsp://"+addr+"/test", sess, true, true)
}

func TestPullStream_WithBasicAuth(t *testing.T) {
	addr := startFakeRTSPServer(t, buildTestSDP(false), []rtsp.InterleavedFrame{
		{Channel: 0, Payload: buildTestVideoRTP()},
	}, "basic")

	trackMux := moqt.NewTrackMux(0)
	sess, err := NewSession(trackMux, "/live/cam")
	require.NoError(t, err)
	defer sess.Close()

	runPullAndVerify(t, addr, "rtsp://admin:secret@"+addr+"/test", sess, true, false)
}

func TestPullStream_WithDigestAuth(t *testing.T) {
	addr := startFakeRTSPServer(t, buildTestSDP(false), []rtsp.InterleavedFrame{
		{Channel: 0, Payload: buildTestVideoRTP()},
	}, "digest")

	trackMux := moqt.NewTrackMux(0)
	sess, err := NewSession(trackMux, "/live/cam")
	require.NoError(t, err)
	defer sess.Close()

	runPullAndVerify(t, addr, "rtsp://admin:secret@"+addr+"/test", sess, true, false)
}

func TestPullStream_VideoOnly(t *testing.T) {
	addr := startFakeRTSPServer(t, buildTestSDP(false), []rtsp.InterleavedFrame{
		{Channel: 0, Payload: buildTestVideoRTP()},
	}, "")

	trackMux := moqt.NewTrackMux(0)
	sess, err := NewSession(trackMux, "/live/cam")
	require.NoError(t, err)
	defer sess.Close()

	runPullAndVerify(t, addr, "rtsp://"+addr+"/test", sess, true, false)
}

// TestPullStream_FUAFragmentation exercises H.264 FU-A reassembly through the
// pull pipeline: two FU-A fragments (start + end) arrive on channel 0 and are
// reassembled into one IDR NAL, then pushed to the Session.
func TestPullStream_FUAFragmentation(t *testing.T) {
	// FU-A: indicator (NRI=3, type=28) + FU header (start/end, type=5) + payload.
	fuaStart := []byte{fuIndicator(3), 0x80 | 5, 0xAA}     // start, type=5
	fuaEnd := []byte{fuIndicator(3), 0x40 | 5, 0xBB, 0xCC} // end, type=5

	addr := startFakeRTSPServer(t, buildTestSDP(false), []rtsp.InterleavedFrame{
		{Channel: 0, Payload: append(buildRTPHeader(96, false, 1, 90000, 0x1111), fuaStart...)},
		{Channel: 0, Payload: append(buildRTPHeader(96, true, 2, 90000, 0x1111), fuaEnd...)},
	}, "")

	trackMux := moqt.NewTrackMux(0)
	sess, err := NewSession(trackMux, "/live/cam")
	require.NoError(t, err)
	defer sess.Close()

	runPullAndVerify(t, addr, "rtsp://"+addr+"/test", sess, true, false)
}

// TestPullStream_SameTimestampAggregation verifies that multiple same-PTS IDR
// NALUs (the ffmpeg RTSP muxer pattern) are aggregated into one AVCC frame.
func TestPullStream_SameTimestampAggregation(t *testing.T) {
	addr := startFakeRTSPServer(t, buildTestSDP(false), []rtsp.InterleavedFrame{
		// Two single-NAL IDRs at the same timestamp; marker on the second.
		{Channel: 0, Payload: append(buildRTPHeader(96, false, 1, 90000, 0x2222), 0x65, 0x01)},
		{Channel: 0, Payload: append(buildRTPHeader(96, true, 2, 90000, 0x2222), 0x65, 0x02)},
	}, "")

	trackMux := moqt.NewTrackMux(0)
	sess, err := NewSession(trackMux, "/live/cam")
	require.NoError(t, err)
	defer sess.Close()

	runPullAndVerify(t, addr, "rtsp://"+addr+"/test", sess, true, false)
}

// TestPullStream_NoSupportedCodecs verifies pullStream returns an error when
// the SDP has no H.264/AAC tracks.
func TestPullStream_NoSupportedCodecs(t *testing.T) {
	sdpBody := "v=0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H265/90000\r\na=control:trackID=0\r\n"
	addr := startFakeRTSPServer(t, sdpBody, nil, "")

	trackMux := moqt.NewTrackMux(0)
	sess, err := NewSession(trackMux, "/live/cam")
	require.NoError(t, err)
	defer sess.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = pullStream(ctx, "rtsp://"+addr+"/test", sess)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no supported media tracks")
}

// TestPullStream_SSRFControlRejected verifies the sameOrigin guard rejects an
// SDP a=control that points to a different host (SSRF via malicious SDP).
func TestPullStream_SSRFControlRejected(t *testing.T) {
	sdpBody := "v=0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=control:rtsp://evil.com/hack\r\n"
	addr := startFakeRTSPServer(t, sdpBody, nil, "")

	trackMux := moqt.NewTrackMux(0)
	sess, err := NewSession(trackMux, "/live/cam")
	require.NoError(t, err)
	defer sess.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = pullStream(ctx, "rtsp://"+addr+"/test", sess)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no supported media tracks",
		"the SSRF track should be rejected, leaving no supported tracks")
}

// TestNewRTSPTrackFromMedia verifies the shared SDP→track helper used by both
// push and pull: H.264 video, AAC audio, and unsupported codecs.
func TestNewRTSPTrackFromMedia(t *testing.T) {
	trackMux := moqt.NewTrackMux(0)
	sess, err := NewSession(trackMux, "/live/test")
	require.NoError(t, err)
	defer sess.Close()

	t.Run("H.264 video with sprop", func(t *testing.T) {
		sps := []byte{0x67, 0x64, 0x00, 0x1F, 0xAC, 0xD9}
		pps := []byte{0x68, 0xEB, 0xE3, 0xCB}
		sprop := base64.StdEncoding.EncodeToString(sps) + "," + base64.StdEncoding.EncodeToString(pps)
		media := &rtsp.SDPMedia{
			Type:   "video",
			RtpMap: "96 H264/90000",
			Fmtp:   "packetization-mode=1; sprop-parameter-sets=" + sprop + "; profile-level-id=64001f",
		}
		track := newRTSPTrackFromMedia(sess, media)
		assert.Equal(t, trackKindVideo, track.kind)
		require.NotNil(t, track.avcCfg)
		assert.NotEmpty(t, track.avcCfg.SPS)
	})

	t.Run("AAC audio with config", func(t *testing.T) {
		media := &rtsp.SDPMedia{
			Type:   "audio",
			RtpMap: "97 mpeg4-generic/48000/2",
			Fmtp:   "streamtype=5; mode=AAC-hbr; sizelength=13; indexlength=3; indexdeltalength=3; config=1190",
		}
		track := newRTSPTrackFromMedia(sess, media)
		assert.Equal(t, trackKindAudio, track.kind)
		require.NotNil(t, track.aacDepack)
	})

	t.Run("unsupported codec (H.265)", func(t *testing.T) {
		media := &rtsp.SDPMedia{
			Type:   "video",
			RtpMap: "96 H265/90000",
		}
		track := newRTSPTrackFromMedia(sess, media)
		assert.Equal(t, trackKind(0), track.kind, "unsupported codec should produce uninitialized track")
		assert.Nil(t, track.avcCfg)
		assert.Nil(t, track.aacDepack)
	})

	t.Run("nil media", func(t *testing.T) {
		track := newRTSPTrackFromMedia(sess, nil)
		assert.Equal(t, trackKind(0), track.kind)
	})
}

// TestStartKeepalive_IntervalClamping verifies the keepalive interval
// calculation: half the session timeout, clamped to [5s, 30s].
func TestStartKeepalive_IntervalClamping(t *testing.T) {
	tests := map[string]struct {
		timeout  time.Duration
		wantMin  time.Duration
		wantMax  time.Duration
	}{
		"60s timeout → 30s":  {60 * time.Second, 30 * time.Second, 30 * time.Second},
		"120s timeout → 30s": {120 * time.Second, 30 * time.Second, 30 * time.Second},  // clamped to max 30s
		"8s timeout → 5s":    {8 * time.Second, 5 * time.Second, 5 * time.Second},       // clamped to min 5s
		"0 (default) → 30s":  {0, 30 * time.Second, 30 * time.Second},                   // default 60s → 30s
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			timeout := tc.timeout
			if timeout == 0 {
				timeout = defaultSessionTimeout
			}
			interval := timeout / 2
			if interval < 5*time.Second {
				interval = 5 * time.Second
			}
			if interval > 30*time.Second {
				interval = 30 * time.Second
			}
			assert.GreaterOrEqual(t, interval, tc.wantMin)
			assert.LessOrEqual(t, interval, tc.wantMax)
		})
	}
}

// TestPullStream_Keepalive verifies that pullStream sends periodic GET_PARAMETER
// keepalives to the RTSP server. It uses a very short timeout to observe at
// least one keepalive within the test window.
func TestPullStream_Keepalive(t *testing.T) {
	keepaliveReceived := make(chan struct{}, 10)

	// A fake server that tracks GET_PARAMETER requests.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		rc := rtsp.NewConn(conn)
		for {
			req, _, err := rc.ReadRequest()
			if err != nil {
				return
			}
			resp := rtsp.NewResponse(rtsp.StatusOK, req)
			if cseq := req.Header.Get("CSeq"); cseq != "" {
				resp.Header.Set("CSeq", cseq)
			}
			switch req.Method {
			case rtsp.MethodDescribe:
				body := []byte(buildTestSDP(false))
				resp.Header.Set("Content-Type", "application/sdp")
				resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
				resp.Body = io.NopCloser(bytes.NewReader(body))
			case rtsp.MethodSetup:
				resp.Header.Set("Transport", req.Header.Get("Transport"))
				// Use a very short timeout so the keepalive goroutine fires quickly.
				resp.Header.Set("Session", "test-keepalive;timeout=10")
			case rtsp.MethodGetParameter:
				keepaliveReceived <- struct{}{}
			case rtsp.MethodPlay:
				_ = rc.WriteResponse(resp)
				// Send one video frame, then keep the connection open.
				f := rtsp.InterleavedFrame{Channel: 0, Payload: buildTestVideoRTP()}
				_ = rc.WriteInterleavedFrame(&f)
				// Keep reading; detect GET_PARAMETER keepalives.
				for {
					req2, _, err := rc.ReadRequest()
					if err != nil {
						return
					}
					if req2 != nil && req2.Method == rtsp.MethodGetParameter {
						keepaliveReceived <- struct{}{}
					}
				}
			}
			_ = rc.WriteResponse(resp)
		}
	}()

	addr := ln.Addr().String()
	trackMux := moqt.NewTrackMux(0)
	sess, err := NewSession(trackMux, "/live/cam")
	require.NoError(t, err)
	defer sess.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go pullStream(ctx, "rtsp://"+addr+"/test", sess)

	// Wait for at least one keepalive to arrive.
	select {
	case <-keepaliveReceived:
		// Success: keepalive was sent.
	case <-time.After(8 * time.Second):
		t.Fatal("expected a GET_PARAMETER keepalive but none arrived")
	}
	cancel()
}
