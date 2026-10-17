package ingest

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/rtsp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minimal H.264 SDP with an SPS/PPS sprop-parameter-sets, as ffmpeg's RTSP
// muxer emits — enough for ANNOUNCE + SETUP to succeed against the ingest
// server.
const testH264SDP = "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=No Name\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\na=tool:libavformat 58.76.100\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=fmtp:96 packetization-mode=1; sprop-parameter-sets=Z2QAH6zZQPARab1g,aOvjyyLA; profile-level-id=4D401F\r\na=control:streamid=0\r\n"

// startRTSPServer stands up an RTSPServer checking pushes with authorize (nil:
// none) on an ephemeral loopback port and returns the bound address once it is
// listening. The server (and its
// ListenAndServe goroutine) are torn down on test cleanup.
func startRTSPServer(t *testing.T, authorize PublishAuthorizer) (*RTSPServer, net.Addr) {
	t.Helper()
	server := NewRTSPServer(RTSPConfig{
		Addr:      "127.0.0.1:0",
		TrackMux:  moqt.NewTrackMux(0),
		Authorize: authorize,
	})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = server.Shutdown(context.Background())
		<-errCh
	})

	var addr net.Addr
	require.Eventually(t, func() bool {
		addr = server.Addr()
		return addr != nil
	}, 2*time.Second, 10*time.Millisecond, "RTSP server never started listening")
	return server, addr
}

// dialRTSP opens a TCP connection to the RTSP server. The connection is closed
// on test cleanup; each call is a fresh connection, so tests/subtests that dial
// independently do not share session state.
func dialRTSP(t *testing.T, addr net.Addr) *rtsp.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr.String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return rtsp.NewConn(c)
}

func mustParseURL(t testing.TB, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	require.NoError(t, err)
	return u
}

// newRequest builds a minimal RTSP request to addr/path with CSeq set.
func newRequest(t *testing.T, method rtsp.Method, addr net.Addr, path string, cseq int) *rtsp.Request {
	t.Helper()
	req := &rtsp.Request{
		Method: method,
		URL:    mustParseURL(t, "rtsp://"+addr.String()+path),
		Proto:  "RTSP/1.0",
		Header: make(map[string][]string),
	}
	req.Header.Set("CSeq", strconv.Itoa(cseq))
	return req
}

// doRequest writes req on rc and returns the parsed response.
func doRequest(t *testing.T, rc *rtsp.Conn, req *rtsp.Request) *rtsp.Response {
	t.Helper()
	require.NoError(t, rc.WriteRequest(req))
	resp, _, err := rc.ReadResponse(req)
	require.NoError(t, err)
	return resp
}

// TestRTSPServer_SessionLifecycle exercises a full RTSP push session
// (OPTIONS → ANNOUNCE → SETUP → RECORD → TEARDOWN) over a single connection.
// It is intentionally one linear test, not subtests: the requests share one
// connection and each depends on the previous succeeding (a real RTSP session
// is stateful), so they are not independent cases. Independent behavior is
// covered by TestRTSPServer_RejectsBadRequests below.
func TestRTSPServer_SessionLifecycle(t *testing.T) {
	_, addr := startRTSPServer(t, nil)
	rc := dialRTSP(t, addr)

	// OPTIONS
	resp := doRequest(t, rc, newRequest(t, rtsp.MethodOptions, addr, "/teststream", 1))
	assert.Equal(t, rtsp.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Public"), "ANNOUNCE")

	// ANNOUNCE (with SDP body)
	req := newRequest(t, rtsp.MethodAnnounce, addr, "/teststream", 2)
	req.Header.Set("Content-Type", "application/sdp")
	req.Header.Set("Content-Length", strconv.Itoa(len(testH264SDP)))
	req.Body = io.NopCloser(strings.NewReader(testH264SDP))
	resp = doRequest(t, rc, req)
	assert.Equal(t, rtsp.StatusOK, resp.StatusCode)

	// SETUP (interleaved transport on the announced track)
	req = newRequest(t, rtsp.MethodSetup, addr, "/teststream/streamid=0", 3)
	req.Header.Set("Transport", "RTP/AVP/TCP;unicast;interleaved=0-1")
	resp = doRequest(t, rc, req)
	assert.Equal(t, rtsp.StatusOK, resp.StatusCode)
	assert.Equal(t, "RTP/AVP/TCP;unicast;interleaved=0-1", resp.Header.Get("Transport"))
	assert.NotEmpty(t, resp.Header.Get("Session"))

	// RECORD
	req = newRequest(t, rtsp.MethodRecord, addr, "/teststream", 4)
	req.Header.Set("Session", "12345678")
	resp = doRequest(t, rc, req)
	assert.Equal(t, rtsp.StatusOK, resp.StatusCode)

	// TEARDOWN
	req = newRequest(t, rtsp.MethodTeardown, addr, "/teststream", 5)
	req.Header.Set("Session", "12345678")
	resp = doRequest(t, rc, req)
	assert.Equal(t, rtsp.StatusOK, resp.StatusCode)
}

// TestRTSPServer_RejectsBadRequests covers the server's request-validation
// paths. Each case is independent: its own connection and a single malformed
// request, so no case depends on another's state.
func TestRTSPServer_RejectsBadRequests(t *testing.T) {
	_, addr := startRTSPServer(t, nil)

	cases := map[string]struct {
		method rtsp.Method
		path   string
		// mutate optionally adjusts the request (e.g. sets headers).
		mutate func(req *rtsp.Request)
		want   int
	}{
		"ANNOUNCE without body or Content-Type": {
			method: rtsp.MethodAnnounce,
			path:   "/s",
			want:   rtsp.StatusBadRequest,
		},
		"SETUP before ANNOUNCE": {
			method: rtsp.MethodSetup,
			path:   "/s/streamid=0",
			mutate: func(r *rtsp.Request) {
				r.Header.Set("Transport", "RTP/AVP/TCP;unicast;interleaved=0-1")
			},
			want: rtsp.StatusBadRequest,
		},
		"unsupported method (DESCRIBE)": {
			method: rtsp.MethodDescribe,
			path:   "/s",
			want:   rtsp.StatusMethodNotAllowed,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rc := dialRTSP(t, addr) // fresh connection — independent of other cases
			req := newRequest(t, tc.method, addr, tc.path, 1)
			if tc.mutate != nil {
				tc.mutate(req)
			}
			resp := doRequest(t, rc, req)
			assert.Equal(t, tc.want, resp.StatusCode)
		})
	}
}

// TestRTSPServer_RejectsNonInterleavedTransport covers the unsupported-transport
// path (461). It needs a successful ANNOUNCE first (SETUP otherwise fails at the
// earlier sdp/session precondition), so it is a two-request sequence rather than
// a single-shot case.
func TestRTSPServer_RejectsNonInterleavedTransport(t *testing.T) {
	_, addr := startRTSPServer(t, nil)
	rc := dialRTSP(t, addr)

	// ANNOUNCE first so SETUP reaches the transport validation.
	req := newRequest(t, rtsp.MethodAnnounce, addr, "/teststream", 1)
	req.Header.Set("Content-Type", "application/sdp")
	req.Header.Set("Content-Length", strconv.Itoa(len(testH264SDP)))
	req.Body = io.NopCloser(strings.NewReader(testH264SDP))
	assert.Equal(t, rtsp.StatusOK, doRequest(t, rc, req).StatusCode)

	// SETUP with a transport that has no "interleaved" token.
	req = newRequest(t, rtsp.MethodSetup, addr, "/teststream/streamid=0", 2)
	req.Header.Set("Transport", "RTP/AVP;unicast") // no interleaved=
	assert.Equal(t, rtsp.StatusUnsupportedTransport, doRequest(t, rc, req).StatusCode)
}

// announce sends an ANNOUNCE of testH264SDP to addr/target on rc and returns
// the response.
func announce(t *testing.T, rc *rtsp.Conn, addr net.Addr, target string) *rtsp.Response {
	t.Helper()
	req := newRequest(t, rtsp.MethodAnnounce, addr, target, 1)
	req.Header.Set("Content-Type", "application/sdp")
	req.Header.Set("Content-Length", strconv.Itoa(len(testH264SDP)))
	req.Body = io.NopCloser(strings.NewReader(testH264SDP))
	return doRequest(t, rc, req)
}

func TestRTSPServer_Authorize(t *testing.T) {
	tests := map[string]struct {
		err        error
		wantStatus int
	}{
		"accepted": {wantStatus: rtsp.StatusOK},
		"refused":  {err: errors.New("no grant"), wantStatus: rtsp.StatusUnauthorized},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			authorize := &fakePublishAuthorizer{err: tt.err}
			_, addr := startRTSPServer(t, authorize.authorize)
			rc := dialRTSP(t, addr)

			resp := announce(t, rc, addr, "/live/cam?jwt=tok")

			assert.Equal(t, tt.wantStatus, resp.StatusCode)
			assert.Equal(t, []publishAsk{{credential: "tok", path: "/live/cam"}}, authorize.questions(),
				"the credential from the URL's query, the path without it")
		})
	}
}

// TestRTSPServer_Authorize_CutsAtLapse verifies a push is cut when its
// permission lapses.
func TestRTSPServer_Authorize_CutsAtLapse(t *testing.T) {
	authorize := &fakePublishAuthorizer{lapses: time.Now().Add(200 * time.Millisecond)}
	_, addr := startRTSPServer(t, authorize.authorize)
	rc := dialRTSP(t, addr)
	require.Equal(t, rtsp.StatusOK, announce(t, rc, addr, "/live/cam?jwt=tok").StatusCode)

	// The server closes the connection, so a request on it fails.
	assert.Eventually(t, func() bool {
		req := newRequest(t, rtsp.MethodOptions, addr, "/live/cam", 2)
		if err := rc.WriteRequest(req); err != nil {
			return true
		}
		_, _, err := rc.ReadResponse(req)
		return err != nil
	}, 3*time.Second, 50*time.Millisecond)
}
