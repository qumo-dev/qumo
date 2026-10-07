package rtmp

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestListen(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		l, err := Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Listen failed: %v", err)
		}
		defer l.Close()

		if l.Addr() == nil {
			t.Fatal("expected non-nil Addr()")
		}

		errCh := make(chan error, 1)
		connCh := make(chan *Conn, 1)

		go func() {
			conn, err := l.Accept()
			if err != nil {
				errCh <- err
				return
			}
			connCh <- conn
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		clientConn, err := Dial(ctx, l.Addr().String())
		if err != nil {
			t.Fatalf("Dial failed: %v", err)
		}
		defer clientConn.Close()

		select {
		case err := <-errCh:
			t.Fatalf("Accept failed: %v", err)
		case conn := <-connCh:
			defer conn.Close()
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for Accept")
		}
	})

	t.Run("error", func(t *testing.T) {
		// Trying to listen on an invalid address should fail
		_, err := Listen("tcp", "invalid-address:-1")
		if err == nil {
			t.Fatal("expected error listening on invalid address")
		}
	})
}

func TestListener_Listen(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		l, err := Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Listen failed: %v", err)
		}
		defer l.Close()

		if l.Addr() == nil {
			t.Fatal("expected non-nil Addr()")
		}

		errCh := make(chan error, 1)
		connCh := make(chan *Conn, 1)

		go func() {
			conn, err := l.Accept()
			if err != nil {
				errCh <- err
				return
			}
			connCh <- conn
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		clientConn, err := Dial(ctx, l.Addr().String())
		if err != nil {
			t.Fatalf("Dial failed: %v", err)
		}
		defer clientConn.Close()

		select {
		case err := <-errCh:
			t.Fatalf("Accept failed: %v", err)
		case conn := <-connCh:
			defer conn.Close()
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for Accept")
		}
	})

	t.Run("error", func(t *testing.T) {
		// Trying to listen on an invalid address should fail
		_, err := Listen("tcp", "invalid-address:-1")
		if err == nil {
			t.Fatal("expected error listening on invalid address")
		}
	})
}

func TestListener_Accept(t *testing.T) {
	l, err := Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		errCh <- err
	}()

	// Wait briefly to ensure Accept is blocking
	time.Sleep(10 * time.Millisecond)

	if err := l.Close(); err != nil {
		t.Errorf("Close failed: %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error from Accept after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for Accept to return after Close")
	}
}

func TestServerConn_HandshakeError(t *testing.T) {
	// Let OS pick a free port.
	l, err := Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer l.Close()

	errCh := make(chan error, 1)

	// Accept in background
	go func() {
		_, err := l.Accept()
		errCh <- err
	}()

	// Connect but do not perform RTMP handshake
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	// Write garbage to fail the handshake
	conn.Write([]byte("not an rtmp handshake"))
	conn.Close()

	// A failed handshake must NOT tear down the listener: Accept loops past it
	// and stays blocked, so a bad client (probe, port scan, half-open conn)
	// can't kill ingest. Give the server time to process the bad handshake —
	// the local TCP close → EOF is handled in well under this window.
	time.Sleep(200 * time.Millisecond)
	select {
	case err := <-errCh:
		t.Fatalf("Accept returned for a failed handshake, killing the listener: %v", err)
	default:
		// Good: the listener is still alive and accepting.
	}

	// Closing the listener is the only legitimate way Accept should return.
	if err := l.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	select {
	case <-errCh:
		// Accept returned on close — expected.
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Accept to return after Close")
	}
}

// TestListener_Accept_StalledHandshakeDoesNotBlock verifies the handshake
// read deadline: a client that connects and then never sends handshake bytes
// cannot hold Accept forever. After the stalled handshake times out, Accept
// loops back and serves a subsequent, well-behaved client.
func TestListener_Accept_StalledHandshakeDoesNotBlock(t *testing.T) {
	// Shorten the handshake timeout so the test does not wait 10s.
	old := handshakeTimeout
	handshakeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = old })

	l, err := Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer l.Close()

	connCh := make(chan *Conn, 1)
	errCh := make(chan error, 1)
	go func() {
		c, aerr := l.Accept()
		if aerr != nil {
			errCh <- aerr
			return
		}
		connCh <- c
	}()

	// A stalling client: connects but never sends handshake bytes.
	stalled, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial stalled client failed: %v", err)
	}
	defer stalled.Close()

	// Wait for the stalled handshake to time out and be skipped.
	time.Sleep(handshakeTimeout + 250*time.Millisecond)

	// A real client now completes the handshake; Accept must return it,
	// proving the stalled client did not permanently block the listener.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	real, err := Dial(ctx, l.Addr().String())
	if err != nil {
		t.Fatalf("Dial real client failed: %v", err)
	}
	defer real.Close()

	select {
	case c := <-connCh:
		_ = c.Close()
	case err := <-errCh:
		t.Fatalf("Accept returned error after stalled handshake: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("Accept never recovered: stalled handshake blocked the listener")
	}
}

// TestListener_Accept_RepeatedStallsDoNotBlock verifies the Accept loop
// recovers from a burst of consecutive bad clients, not just one. Each must be
// skipped independently; a loop that only recovered once (e.g. drained a
// one-slot error path) would pass the single-stall test above but fail here.
func TestListener_RepeatedStallsDoNotBlock(t *testing.T) {
	old := handshakeTimeout
	handshakeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = old })

	l, err := Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer l.Close()

	connCh := make(chan *Conn, 1)
	errCh := make(chan error, 1)
	go func() {
		c, aerr := l.Accept()
		if aerr != nil {
			errCh <- aerr
			return
		}
		connCh <- c
	}()

	// A burst of clients that each connect, send malformed handshake bytes,
	// and close — every one must fail the handshake and be skipped.
	const badBurst = 5
	for i := range badBurst {
		c, derr := net.Dial("tcp", l.Addr().String())
		if derr != nil {
			t.Fatalf("dial bad client %d failed: %v", i, derr)
		}
		_, _ = c.Write([]byte("not an rtmp handshake"))
		_ = c.Close()
	}
	// Plus a staller that connects and sends nothing — exercising both the
	// immediate-failure and the timeout-failure paths back-to-back.
	stalled, serr := net.Dial("tcp", l.Addr().String())
	if serr != nil {
		t.Fatalf("dial stalled client failed: %v", serr)
	}
	defer stalled.Close()

	// Give the loop time to process the burst + the stall timeout.
	time.Sleep(handshakeTimeout + 250*time.Millisecond)

	// A well-behaved client must still be served, proving none of the bad
	// clients fatally wedged the listener.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	real, derr := Dial(ctx, l.Addr().String())
	if derr != nil {
		t.Fatalf("Dial real client failed: %v", derr)
	}
	defer real.Close()

	select {
	case c := <-connCh:
		_ = c.Close()
	case err := <-errCh:
		t.Fatalf("Accept returned error after bad-client burst: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("Accept never recovered: a bad client in the burst fatally blocked the listener")
	}
}
