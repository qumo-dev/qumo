package rtsp

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseInterleaved(t *testing.T) {
	tests := map[string]struct {
		transport string
		rtp       uint8
		rtcp      uint8
		ok        bool
	}{
		"pair":      {"RTP/AVP/TCP;unicast;interleaved=0-1", 0, 1, true},
		"pair 2":    {"interleaved=2-3", 2, 3, true},
		"single":    {"interleaved=4", 4, 4, true},
		"with mode": {"RTP/AVP/TCP;unicast;mode=PLAY;interleaved=6-7", 6, 7, true},
		"absent":    {"RTP/AVP;unicast", 0, 0, false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rtp, rtcp, ok := parseInterleaved(tc.transport)
			assert.Equal(t, tc.ok, ok)
			if ok {
				assert.Equal(t, tc.rtp, rtp)
				assert.Equal(t, tc.rtcp, rtcp)
			}
		})
	}
}

func TestSelectQop(t *testing.T) {
	assert.Equal(t, "auth", selectQop("auth"))
	assert.Equal(t, "auth", selectQop("auth,auth-int"))
	assert.Equal(t, "auth", selectQop("auth-int,auth"))
	assert.Equal(t, "", selectQop("auth-int")) // not supported → legacy
	assert.Equal(t, "", selectQop(""))         // no qop
}

func TestDial_InvalidScheme(t *testing.T) {
	_, err := Dial(context.Background(), "http://example.com/stream")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not an rtsp url")
}

func TestDial_MalformedURL(t *testing.T) {
	_, err := Dial(context.Background(), "://bad")
	require.Error(t, err)
}

// startFakeServer starts a TCP listener that runs fn for each connection,
// giving fn a raw *bufio.ReadWriter to read requests / write responses. It
// returns the listener address.
func startFakeServer(t *testing.T, fn func(rw *bufio.ReadWriter)) string {
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
		fn(bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn)))
	}()
	return ln.Addr().String()
}

// writeRTSPResponse writes a minimal RTSP response on a raw bufio.ReadWriter.
func writeRTSPResponse(rw *bufio.ReadWriter, statusCode int, cseq string, extraHeaders ...string) {
	fmt.Fprintf(rw, "RTSP/1.0 %d %s\r\n", statusCode, statusText(statusCode))
	fmt.Fprintf(rw, "CSeq: %s\r\n", cseq)
	for _, h := range extraHeaders {
		fmt.Fprintf(rw, "%s\r\n", h)
	}
	fmt.Fprintf(rw, "\r\n")
	rw.Flush()
}

func TestClient_DescribeReturnsErrorOn404(t *testing.T) {
	addr := startFakeServer(t, func(rw *bufio.ReadWriter) {
		ReadRequest(rw.Reader) // DESCRIBE (client sends no OPTIONS)
		writeRTSPResponse(rw, 404, "1")
	})

	client, err := Dial(context.Background(), "rtsp://"+addr+"/test")
	require.NoError(t, err)
	defer client.Close()

	_, err = client.Describe(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DESCRIBE")
}

func TestClient_SetupReturnsErrorOn461(t *testing.T) {
	addr := startFakeServer(t, func(rw *bufio.ReadWriter) {
		// DESCRIBE → 200 + SDP.
		ReadRequest(rw.Reader)
		sdp := "v=0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=control:trackID=0\r\n"
		fmt.Fprintf(rw, "RTSP/1.0 200 OK\r\nCSeq: 1\r\nContent-Type: application/sdp\r\nContent-Length: %d\r\n\r\n%s",
			len(sdp), sdp)
		rw.Flush()

		// SETUP → 461 (unsupported transport).
		ReadRequest(rw.Reader)
		writeRTSPResponse(rw, 461, "2")
	})

	client, err := Dial(context.Background(), "rtsp://"+addr+"/test")
	require.NoError(t, err)
	defer client.Close()

	_, err = client.Describe(context.Background())
	require.NoError(t, err)

	_, err = client.Setup(context.Background(), "rtsp://"+addr+"/test/trackID=0", 0, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SETUP")
}

func TestParseSessionTimeout(t *testing.T) {
	tests := map[string]struct {
		session string
		want    time.Duration
	}{
		"with timeout":         {"12345;timeout=60", 60 * time.Second},
		"timeout only":         {";timeout=30", 30 * time.Second},
		"no timeout":           {"12345", 0},
		"empty":                {"", 0},
		"timeout with spaces":  {"12345; timeout = 120", 120 * time.Second},
		"zero timeout":         {"12345;timeout=0", 0},
		"invalid timeout":      {"12345;timeout=abc", 0},
		"multiple params":      {"12345;timeout=45;foo=bar", 45 * time.Second},
		"case insensitive key": {"12345;Timeout=90", 90 * time.Second},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := parseSessionTimeout(tc.session)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestClient_SetupCapturesSessionTimeout(t *testing.T) {
	addr := startFakeServer(t, func(rw *bufio.ReadWriter) {
		// DESCRIBE → 200 + SDP.
		ReadRequest(rw.Reader)
		sdp := "v=0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=control:trackID=0\r\n"
		fmt.Fprintf(rw, "RTSP/1.0 200 OK\r\nCSeq: 1\r\nContent-Type: application/sdp\r\nContent-Length: %d\r\n\r\n%s",
			len(sdp), sdp)
		rw.Flush()

		// SETUP → 200 with Session + timeout.
		ReadRequest(rw.Reader)
		writeRTSPResponse(rw, 200, "2",
			"Transport: RTP/AVP/TCP;unicast;interleaved=0-1",
			"Session: abcdef;timeout=45")
	})

	client, err := Dial(context.Background(), "rtsp://"+addr+"/test")
	require.NoError(t, err)
	defer client.Close()

	_, err = client.Describe(context.Background())
	require.NoError(t, err)

	_, err = client.Setup(context.Background(), "rtsp://"+addr+"/test/trackID=0", 0, 1)
	require.NoError(t, err)

	assert.Equal(t, 45*time.Second, client.SessionTimeout())
}

func TestClient_SendKeepalive(t *testing.T) {
	var gotMethod, gotSession string
	addr := startFakeServer(t, func(rw *bufio.ReadWriter) {
		// DESCRIBE → 200 + SDP.
		ReadRequest(rw.Reader)
		sdp := "v=0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=control:trackID=0\r\n"
		fmt.Fprintf(rw, "RTSP/1.0 200 OK\r\nCSeq: 1\r\nContent-Type: application/sdp\r\nContent-Length: %d\r\n\r\n%s",
			len(sdp), sdp)
		rw.Flush()

		// SETUP → 200 with Session.
		ReadRequest(rw.Reader)
		writeRTSPResponse(rw, 200, "2",
			"Transport: RTP/AVP/TCP;unicast;interleaved=0-1",
			"Session: mysess;timeout=60")

		// Read the keepalive request.
		req, err := ReadRequest(rw.Reader)
		if err != nil {
			return
		}
		gotMethod = string(req.Method)
		gotSession = req.Header.Get("Session")
		writeRTSPResponse(rw, 200, req.Header.Get("CSeq"))
	})

	client, err := Dial(context.Background(), "rtsp://"+addr+"/test")
	require.NoError(t, err)
	defer client.Close()

	_, err = client.Describe(context.Background())
	require.NoError(t, err)

	_, err = client.Setup(context.Background(), "rtsp://"+addr+"/test/trackID=0", 0, 1)
	require.NoError(t, err)

	err = client.SendKeepalive()
	require.NoError(t, err)

	// Give the server goroutine time to process.
	time.Sleep(50 * time.Millisecond)

	assert.Equal(t, "GET_PARAMETER", gotMethod)
	assert.Equal(t, "mysess", gotSession)
}
