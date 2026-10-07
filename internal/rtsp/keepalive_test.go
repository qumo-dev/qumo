package rtsp

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testFrame is one interleaved frame on channel 0 as it appears on the wire,
// and the payload a client should read out of it.
const (
	testFrame        = "$\x00\x00\x03abc"
	testFramePayload = "abc"
)

// dialTest connects a Client to addr, carrying userinfo when it is non-empty.
func dialTest(tb testing.TB, addr, userinfo string) *Client {
	tb.Helper()
	if userinfo != "" {
		userinfo += "@"
	}
	client, err := Dial(context.Background(), "rtsp://"+userinfo+addr+"/test")
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = client.Close() })
	return client
}

func TestClient_ReadInterleaved_ControlMessages(t *testing.T) {
	tests := map[string]struct {
		// message is what the server writes on the control channel before the frame.
		message string
	}{
		"response with a one-word reason": {
			message: "RTSP/1.0 200 OK\r\nCSeq: 4\r\n\r\n",
		},
		"response with a reason of several words": {
			message: "RTSP/1.0 405 Method Not Allowed\r\nCSeq: 4\r\n\r\n",
		},
		"response with a body": {
			message: "RTSP/1.0 200 OK\r\nCSeq: 4\r\nContent-Length: 12\r\n\r\npacket_count",
		},
		"error response with a body that looks like nothing": {
			message: "RTSP/1.0 404 Not Found\r\nCSeq: 4\r\nContent-Length: 9\r\n\r\n$not here",
		},
		"request from the server": {
			message: "OPTIONS rtsp://camera/test RTSP/1.0\r\nCSeq: 1\r\n\r\n",
		},
		"request from the server with a body": {
			message: "SET_PARAMETER rtsp://camera/test RTSP/1.0\r\nCSeq: 1\r\nContent-Length: 5\r\n\r\nx: 12",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			addr := startFakeServer(t, func(rw *bufio.ReadWriter) {
				fmt.Fprint(rw, tt.message, testFrame)
				rw.Flush()
			})
			client := dialTest(t, addr, "")

			frame, err := client.ReadInterleaved()

			require.NoError(t, err)
			assert.Equal(t, testFramePayload, string(frame.Payload))
		})
	}
}

func TestClient_ReadInterleaved_SessionNotFound(t *testing.T) {
	addr := startFakeServer(t, func(rw *bufio.ReadWriter) {
		fmt.Fprint(rw, "RTSP/1.0 454 Session Not Found\r\nCSeq: 4\r\n\r\n", testFrame)
		rw.Flush()
	})
	client := dialTest(t, addr, "")

	frame, err := client.ReadInterleaved()

	assert.ErrorIs(t, err, ErrSessionNotFound)
	assert.Nil(t, frame)
}

// keepaliveSeen is what a fake server saw of one keepalive request.
type keepaliveSeen struct {
	method        Method
	authorization string
}

// serveKeepalives answers the first keepalive with firstAnswer followed by one
// frame, and reports every keepalive it reads on seen.
func serveKeepalives(t *testing.T, firstAnswer string, seen chan<- keepaliveSeen) string {
	t.Helper()
	return startFakeServer(t, func(rw *bufio.ReadWriter) {
		for first := true; ; first = false {
			req, err := ReadRequest(rw.Reader)
			if err != nil {
				return
			}
			seen <- keepaliveSeen{
				method:        req.Method,
				authorization: req.Header.Get("Authorization"),
			}
			if first {
				fmt.Fprint(rw, firstAnswer, testFrame)
				rw.Flush()
			}
		}
	})
}

// nextKeepalive waits for the server to report a keepalive.
func nextKeepalive(tb testing.TB, seen <-chan keepaliveSeen) keepaliveSeen {
	tb.Helper()
	select {
	case got := <-seen:
		return got
	case <-time.After(5 * time.Second):
		require.FailNow(tb, "the server saw no keepalive")
		return keepaliveSeen{}
	}
}

func TestClient_SendKeepalive_FallsBackToOptions(t *testing.T) {
	tests := map[string]struct {
		answer string
	}{
		"method not allowed": {answer: "RTSP/1.0 405 Method Not Allowed\r\nCSeq: 1\r\n\r\n"},
		"not implemented":    {answer: "RTSP/1.0 501 Not Implemented\r\nCSeq: 1\r\n\r\n"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			seen := make(chan keepaliveSeen, 2)
			client := dialTest(t, serveKeepalives(t, tt.answer, seen), "")
			require.NoError(t, client.SendKeepalive())
			first := nextKeepalive(t, seen)
			// Reading up to the frame takes in the refusal on the way.
			_, err := client.ReadInterleaved()
			require.NoError(t, err)

			require.NoError(t, client.SendKeepalive())

			assert.Equal(t, MethodGetParameter, first.method)
			assert.Equal(t, MethodOptions, nextKeepalive(t, seen).method)
		})
	}
}

func TestClient_SendKeepalive_AnswersChallenge(t *testing.T) {
	seen := make(chan keepaliveSeen, 2)
	challenge := "RTSP/1.0 401 Unauthorized\r\nCSeq: 1\r\nWWW-Authenticate: Basic realm=\"camera\"\r\n\r\n"
	client := dialTest(t, serveKeepalives(t, challenge, seen), "user:secret")
	require.NoError(t, client.SendKeepalive())
	first := nextKeepalive(t, seen)
	// Reading up to the frame takes in the challenge on the way.
	_, err := client.ReadInterleaved()
	require.NoError(t, err)

	require.NoError(t, client.SendKeepalive())

	assert.Empty(t, first.authorization)
	// "user:secret" in Base64.
	assert.Equal(t, "Basic dXNlcjpzZWNyZXQ=", nextKeepalive(t, seen).authorization)
}

func TestClient_SendKeepalive_UnchallengedCarriesNoAuthorization(t *testing.T) {
	seen := make(chan keepaliveSeen, 1)
	client := dialTest(t, serveKeepalives(t, "", seen), "user:secret")

	require.NoError(t, client.SendKeepalive())

	assert.Empty(t, nextKeepalive(t, seen).authorization)
}

// A keepalive is sent from its own goroutine while the session is being closed
// from another; both number their requests. Run with -race, this fails if the
// numbering is not guarded.
func TestClient_SendKeepalive_ConcurrentWithClose(t *testing.T) {
	seen := make(chan keepaliveSeen, 64)
	client := dialTest(t, serveKeepalives(t, "", seen), "")
	client.sessionID = "abc" // as after SETUP: Close then sends TEARDOWN.

	var wg sync.WaitGroup
	wg.Go(func() {
		for range 20 {
			// not actionable: once Close has run the write fails, which is
			// the interleaving under test and not a failure of it.
			_ = client.SendKeepalive()
		}
	})
	wg.Go(func() { _ = client.Close() })
	wg.Wait()

	assert.LessOrEqual(t, client.nextCSeq(), 22, "each request takes one sequence number")
}

func TestConn_WriteRequestBy_GivesUpAtDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// low-level utility: net.Pipe stands in for a peer that never reads,
		// which is the one condition under which a write blocks.
		near, far := net.Pipe()
		defer near.Close()
		defer far.Close()
		conn := NewConn(near)
		client := &Client{conn: conn, url: mustParseURL("rtsp://camera/test")}
		req := client.newRequest(MethodGetParameter, "rtsp://camera/test")

		err := conn.writeRequestBy(req, time.Now().Add(controlWriteTimeout))

		assert.ErrorIs(t, err, os.ErrDeadlineExceeded)
	})
}
