package rtmp

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChunkBasicHeaderRoundTrip verifies all three basic-header encodings.
func TestChunkBasicHeaderRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		csid chunkStreamID
		fmt  uint8
	}{
		{"1-byte csid=2", 2, 0},
		{"1-byte csid=63", 63, 1},
		{"2-byte csid=64", 64, 2},
		{"2-byte csid=319", 319, 3},
		{"3-byte csid=320", 320, 0},
		{"3-byte csid=65599", 65599, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			orig := chunkBasicHeader{fmt: tc.fmt, chunkStreamID: tc.csid}
			if err := orig.encode(&buf); err != nil {
				t.Fatal(err)
			}
			var dec chunkBasicHeader
			if err := dec.decode(&buf); err != nil {
				t.Fatal(err)
			}
			if dec.fmt != orig.fmt || dec.chunkStreamID != orig.chunkStreamID {
				t.Fatalf("got fmt=%d csid=%d; want fmt=%d csid=%d",
					dec.fmt, dec.chunkStreamID, orig.fmt, orig.chunkStreamID)
			}
		})
	}
}

// TestMessageRoundTrip exercises the full path: write chunks → read message.
func TestMessageRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, 300) // larger than default chunk size (128)

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	writerConn := newConn(client)
	readerConn := newConn(server)

	errCh := make(chan error, 1)
	go func() {
		errCh <- writerConn.writeRawMessage(csidVideo, &rawMessage{
			typeID:    messageTypeVideo,
			streamID:  1,
			timestamp: 1000,
			payload:   payload,
		})
	}()

	msg, err := readerConn.readMessage()
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("writeRawMessage: %v", err)
	}

	if msg.typeID != messageTypeVideo {
		t.Fatalf("typeID = %d; want %d", msg.typeID, messageTypeVideo)
	}
	if msg.streamID != 1 {
		t.Fatalf("streamID = %d; want 1", msg.streamID)
	}
	if msg.timestamp != 1000 {
		t.Fatalf("timestamp = %d; want 1000", msg.timestamp)
	}
	if !bytes.Equal(msg.payload, payload) {
		t.Fatalf("payload length = %d; want %d", len(msg.payload), len(payload))
	}
}

// TestExtendedTimestampRoundTrip verifies that timestamps >= 0xFFFFFF are
// encoded and decoded correctly using the extended timestamp field.
func TestExtendedTimestampRoundTrip(t *testing.T) {
	payload := []byte{0x01, 0x02, 0x03}

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	writerConn := newConn(client)
	readerConn := newConn(server)

	ts := uint32(0x01000000) // > 0xFFFFFF

	errCh := make(chan error, 1)
	go func() {
		errCh <- writerConn.writeRawMessage(csidAudio, &rawMessage{
			typeID:    messageTypeAudio,
			streamID:  1,
			timestamp: ts,
			payload:   payload,
		})
	}()

	msg, err := readerConn.readMessage()
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("writeRawMessage: %v", err)
	}

	if msg.timestamp != ts {
		t.Fatalf("timestamp = 0x%X; want 0x%X", msg.timestamp, ts)
	}
}

// TestSetChunkSizeHandling verifies that a SetChunkSize control message
// updates the reader's chunk size so that subsequent large messages decode.
func TestSetChunkSizeHandling(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	writerConn := newConn(client)
	readerConn := newConn(server)

	errCh := make(chan error, 1)

	go func() {
		// Send SetChunkSize(512).
		var buf bytes.Buffer
		(&messageSetChunkSize{ChunkSize: 512}).encode(&buf)
		if err := writerConn.writeRawMessage(csidControl, &rawMessage{
			typeID:  messageTypeSetChunkSize,
			payload: buf.Bytes(),
		}); err != nil {
			errCh <- err
			return
		}
		writerConn.writeChunkSize = 512

		// Send payload larger than old chunk size (128) but fits in new (512).
		payload := bytes.Repeat([]byte{0xCD}, 400)
		errCh <- writerConn.writeRawMessage(csidVideo, &rawMessage{
			typeID:    messageTypeVideo,
			streamID:  1,
			timestamp: 100,
			payload:   payload,
		})
	}()

	// Read SetChunkSize.
	msg, err := readerConn.readMessage()
	if err != nil {
		t.Fatalf("readMessage (SetChunkSize): %v", err)
	}
	if err := readerConn.handleControlMessage(msg); err != nil {
		t.Fatalf("handleControlMessage: %v", err)
	}
	if readerConn.readChunkSize != 512 {
		t.Fatalf("readChunkSize = %d; want 512", readerConn.readChunkSize)
	}

	// Read data message.
	msg, err = readerConn.readMessage()
	if err != nil {
		t.Fatalf("readMessage (video): %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("writer error: %v", err)
	}
	if len(msg.payload) != 400 {
		t.Fatalf("payload len = %d; want 400", len(msg.payload))
	}
}

// TestServerClientNegotiation exercises AcceptStream / OpenStream end-to-end.
func TestServerClientNegotiation(t *testing.T) {
	serverTransport, clientTransport := net.Pipe()
	defer serverTransport.Close()
	defer clientTransport.Close()

	serverConn := newConn(serverTransport)
	clientConn := newConn(clientTransport)

	type readerResult struct {
		reader *MessageReader
		err    error
	}
	type writerResult struct {
		writer *MessageWriter
		err    error
	}

	acceptCh := make(chan readerResult, 1)
	openCh := make(chan writerResult, 1)

	go func() {
		r, err := serverConn.AcceptStream()
		acceptCh <- readerResult{r, err}
	}()
	go func() {
		w, err := clientConn.OpenStream("live", "test-key")
		openCh <- writerResult{w, err}
	}()

	ar := <-acceptCh
	if ar.err != nil {
		t.Fatalf("AcceptStream: %v", ar.err)
	}
	or := <-openCh
	if or.err != nil {
		t.Fatalf("OpenStream: %v", or.err)
	}

	if ar.reader.StreamKey() != "test-key" {
		t.Fatalf("StreamKey = %q; want %q", ar.reader.StreamKey(), "test-key")
	}

	// Send a video frame from client and read it on server.
	frame := &Frame{
		Type:      FrameTypeVideo,
		Timestamp: 100,
		Data:      []byte{0x17, 0x01, 0x00, 0x00, 0x00},
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- or.writer.WriteFrame(frame)
	}()

	got, err := ar.reader.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if got.Type != FrameTypeVideo {
		t.Fatalf("frame type = %d; want %d", got.Type, FrameTypeVideo)
	}
	if got.Timestamp != 100 {
		t.Fatalf("frame timestamp = %d; want 100", got.Timestamp)
	}
	if !bytes.Equal(got.Data, frame.Data) {
		t.Fatalf("frame data mismatch")
	}
}

// TestHandshakeIntegration tests the full handshake over net.Pipe.
func TestHandshakeIntegration(t *testing.T) {
	serverTransport, clientTransport := net.Pipe()
	defer serverTransport.Close()
	defer clientTransport.Close()

	errCh := make(chan error, 2)
	var serverC, clientC *Conn

	go func() {
		c, err := ServerConn(serverTransport)
		serverC = c
		errCh <- err
	}()
	go func() {
		c, err := ClientConn(clientTransport)
		clientC = c
		errCh <- err
	}()

	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatalf("handshake error: %v", err)
		}
	}

	if serverC == nil || clientC == nil {
		t.Fatal("conn is nil after handshake")
	}

	_ = serverC.Close()
	_ = clientC.Close()
}

// TestZeroLengthMessage verifies that a zero-byte payload message round-trips.
func TestZeroLengthMessage(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	writerConn := newConn(client)
	readerConn := newConn(server)

	errCh := make(chan error, 1)
	go func() {
		errCh <- writerConn.writeRawMessage(csidControl, &rawMessage{
			typeID:  messageTypeUserControl,
			payload: nil,
		})
	}()

	msg, err := readerConn.readMessage()
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("writeRawMessage: %v", err)
	}
	if msg.typeID != messageTypeUserControl {
		t.Fatalf("typeID = %d; want %d", msg.typeID, messageTypeUserControl)
	}
	if len(msg.payload) != 0 {
		t.Fatalf("payload length = %d; want 0", len(msg.payload))
	}
}

// TestMultipleMessagesOnSameChunkStream tests reading several messages
// on the same chunk stream (exercises fmt 3 continuation for new messages).
func TestMultipleMessagesOnSameChunkStream(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	writerConn := newConn(client)
	readerConn := newConn(server)

	payloads := [][]byte{
		bytes.Repeat([]byte{0x01}, 50),
		bytes.Repeat([]byte{0x02}, 50),
		bytes.Repeat([]byte{0x03}, 50),
	}

	errCh := make(chan error, 1)
	go func() {
		for i, p := range payloads {
			if err := writerConn.writeRawMessage(csidAudio, &rawMessage{
				typeID:    messageTypeAudio,
				streamID:  1,
				timestamp: uint32(i * 100),
				payload:   p,
			}); err != nil {
				errCh <- err
				return
			}
		}
		errCh <- nil
	}()

	for i, want := range payloads {
		msg, err := readerConn.readMessage()
		if err != nil {
			t.Fatalf("readMessage[%d]: %v", i, err)
		}
		if !bytes.Equal(msg.payload, want) {
			t.Fatalf("payload[%d] mismatch", i)
		}
	}

	if err := <-errCh; err != nil {
		t.Fatalf("writer error: %v", err)
	}
}

// BenchmarkMessageRoundTrip measures the throughput of writing and reading
// a realistic-sized video frame over the chunk layer.
func BenchmarkMessageRoundTrip(b *testing.B) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	writerConn := newConn(client)
	writerConn.writeChunkSize = 4096
	readerConn := newConn(server)
	readerConn.readChunkSize = 4096

	payload := make([]byte, 4000)
	for i := range payload {
		payload[i] = byte(i)
	}

	errCh := make(chan error, 1)
	go func() {
		for range b.N {
			if err := writerConn.writeRawMessage(csidVideo, &rawMessage{
				typeID:    messageTypeVideo,
				streamID:  1,
				timestamp: 33,
				payload:   payload,
			}); err != nil {
				errCh <- err
				return
			}
		}
		errCh <- nil
	}()

	b.SetBytes(int64(len(payload)))
	b.ResetTimer()

	for range b.N {
		msg, err := readerConn.readMessage()
		if err != nil {
			b.Fatal(err)
		}
		_ = msg
	}

	if err := <-errCh; err != nil {
		b.Fatal(err)
	}
}

// devNull satisfies io.Reader for handshake short-circuit scenarios.
var _ io.Reader = (*countingReader)(nil)

// TestMessageReader_Close verifies sending stream EOF and closing connection.
func TestMessageReader_Close(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	readerConn := newConn(client)
	reader := &MessageReader{conn: readerConn, streamID: 1, app: "live", streamKey: "key"}

	errCh := make(chan error, 1)
	go func() {
		errCh <- reader.Close()
	}()

	// The reader will send a user control message StreamEOF.
	serverConn := newConn(server)
	msg, err := serverConn.readMessage()
	if err != nil {
		t.Fatalf("expected message, got %v", err)
	}
	if msg.typeID != messageTypeUserControl {
		t.Fatalf("expected messageTypeUserControl, got %v", msg.typeID)
	}

	var uc messageUserControl
	if err := uc.decode(bytes.NewReader(msg.payload)); err != nil {
		t.Fatalf("decode user control: %v", err)
	}
	if uc.EventType != userControlStreamEOF {
		t.Fatalf("expected EventType streamEOF, got %v", uc.EventType)
	}

	// Since we close the underlying connection, reading again should give an error.
	_, err = serverConn.readMessage()
	if err == nil {
		t.Fatalf("expected error reading after close, got nil")
	}

	if err := <-errCh; err != nil {
		t.Fatalf("Close returned err: %v", err)
	}
}

// TestMessageWriter_Close verifies sending deleteStream and closing connection.
func TestMessageWriter_Close(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	writerConn := newConn(client)
	writer := &MessageWriter{conn: writerConn, streamID: 1, app: "live", streamKey: "key"}

	errCh := make(chan error, 1)
	go func() {
		errCh <- writer.Close()
	}()

	// The writer will send an AMF0 command deleteStream.
	serverConn := newConn(server)
	msg, err := serverConn.readMessage()
	if err != nil {
		t.Fatalf("expected message, got %v", err)
	}
	if msg.typeID != messageTypeAMF0Command {
		t.Fatalf("expected messageTypeAMF0Command, got %v", msg.typeID)
	}

	name, _, _, err := serverConn.readCommand(msg)
	if err != nil {
		t.Fatalf("readCommand: %v", err)
	}
	if name != commandMessageNameDeleteStream {
		t.Fatalf("expected deleteStream, got %v", name)
	}

	// Since we close the underlying connection, reading again should give an error.
	_, err = serverConn.readMessage()
	if err == nil {
		t.Fatalf("expected error reading after close, got nil")
	}

	if err := <-errCh; err != nil {
		t.Fatalf("Close returned err: %v", err)
	}
}

func TestConn_AcceptStream_Errors(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	conn := newConn(server)
	client.Close() // Cause readMessage to fail
	_, err := conn.AcceptStream()
	if err == nil {
		t.Fatalf("expected error from AcceptStream on closed connection")
	}
}

func TestConn_OpenStream_Errors(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	conn := newConn(server)
	client.Close() // Cause read/write to fail
	_, err := conn.OpenStream("live", "test")
	if err == nil {
		t.Fatalf("expected error from OpenStream on closed connection")
	}
}

// TestConn_LocalRemoteAddr using a real tcp connection to ensure non-nil values
func TestConn_LocalRemoteAddrReal(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer l.Close()

	client, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer client.Close()

	conn := newConn(client)
	if conn.LocalAddr() == nil {
		t.Fatalf("expected non-nil LocalAddr")
	}
	if conn.RemoteAddr() == nil {
		t.Fatalf("expected non-nil RemoteAddr")
	}
}

func TestConn_AcceptStreamFunc(t *testing.T) {
	refused := errors.New("no credential")
	tests := map[string]struct {
		allowErr error
		wantErr  error
	}{
		"allowed": {},
		"refused": {allowErr: refused, wantErr: ErrPublishDenied},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			serverTransport, clientTransport := net.Pipe()
			defer serverTransport.Close()
			defer clientTransport.Close()
			serverConn := newConn(serverTransport)
			clientConn := newConn(clientTransport)
			var gotApp, gotKey string
			accepted := make(chan error, 1)
			go func() {
				_, err := serverConn.AcceptStreamFunc(func(app, streamKey string) error {
					gotApp, gotKey = app, streamKey
					return tt.allowErr
				})
				accepted <- err
			}()

			_, err := clientConn.OpenStream("live", "cam?jwt=t")
			require.NoError(t, err)
			// The client reads no status after publishing; drain what the
			// server answers so its write on the pipe completes.
			go func() { _, _ = io.Copy(io.Discard, clientTransport) }()
			err = <-accepted

			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				assert.ErrorIs(t, err, tt.allowErr, "the check's reason is kept")
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, "live", gotApp)
			assert.Equal(t, "cam?jwt=t", gotKey, "the stream key as published, query and all")
		})
	}
}
