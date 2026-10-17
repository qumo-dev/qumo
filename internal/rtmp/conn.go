package rtmp

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/qumo-dev/qumo/internal/rtmp/amf0"
)

const (
	defaultReadChunkSize  uint32 = 128
	defaultWriteChunkSize uint32 = 128
	serverChunkSize       uint32 = 4096
	defaultWindowAckSize  uint32 = 2500000
	maxMessageSize        uint32 = 16 * 1024 * 1024
)

const (
	csidControl chunkStreamID = 2
	csidCommand chunkStreamID = 3
	csidAudio   chunkStreamID = 6
	csidVideo   chunkStreamID = 7
)

const (
	userControlStreamBegin  uint16 = 0
	userControlStreamEOF    uint16 = 1
	userControlPingRequest  uint16 = 6
	userControlPingResponse uint16 = 7
)

// messageStreamID identifies a logical message stream within an RTMP connection.
// Stream ID 0 is reserved for the control stream; media streams use IDs
// allocated by createStream commands.
type messageStreamID uint32

// countingReader wraps an io.Reader and counts bytes read.
type countingReader struct {
	r     io.Reader
	count atomic.Uint64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.count.Add(uint64(n))
	return n, err
}

// countingWriter wraps an io.Writer and counts bytes written.
type countingWriter struct {
	w     io.Writer
	count atomic.Uint64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.count.Add(uint64(n))
	return n, err
}

// chunkReadState tracks per-chunk-stream state for message reassembly.
type chunkReadState struct {
	timestamp       uint32
	timestampDelta  uint32
	messageLength   uint32
	messageTypeID   uint8
	messageStreamID uint32
	hasExtended     bool

	payload   []byte
	remaining uint32
}

// rawMessage is a fully reassembled RTMP message.
type rawMessage struct {
	typeID    messageTypeID
	streamID  messageStreamID
	timestamp uint32
	payload   []byte
}

func newConn(transport net.Conn) *Conn {
	cr := &countingReader{r: transport}
	cw := &countingWriter{w: transport}
	return &Conn{
		transport:      transport,
		counter:        cr,
		writeCounter:   cw,
		br:             bufio.NewReaderSize(cr, 4096),
		bw:             bufio.NewWriterSize(cw, 4096),
		readChunkSize:  defaultReadChunkSize,
		writeChunkSize: defaultWriteChunkSize,
		readStates:     make(map[chunkStreamID]*chunkReadState),
		windowAckSize:  defaultWindowAckSize,
		nextStreamID:   1,
	}
}

// Conn represents an RTMP connection over a TCP transport.
// After the handshake, use [Conn.AcceptStream] (server) or
// [Conn.OpenStream] (client) to negotiate a media stream.
type Conn struct {
	transport    net.Conn
	counter      *countingReader
	writeCounter *countingWriter
	br           *bufio.Reader
	bw           *bufio.Writer
	writeMu      sync.Mutex

	readChunkSize  uint32
	writeChunkSize uint32

	readStates map[chunkStreamID]*chunkReadState

	windowAckSize  uint32
	lastAcked      uint64
	peerAckedBytes uint64 // last sequence number acknowledged by the peer

	nextStreamID uint32
}

// ---------------------------------------------------------------------------
// Low-level chunk I/O
// ---------------------------------------------------------------------------

// readMessage reads chunks from the transport and returns one complete message.
func (c *Conn) readMessage() (*rawMessage, error) {
	for {
		var bh chunkBasicHeader
		if err := bh.decode(c.br); err != nil {
			return nil, err
		}

		state := c.readStates[bh.chunkStreamID]
		if state == nil {
			state = &chunkReadState{}
			c.readStates[bh.chunkStreamID] = state
		}

		switch bh.fmt {
		case 0: // fmt 0 – full header (11 bytes)
			var hdr [11]byte
			if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
				return nil, err
			}
			ts := uint32(hdr[0])<<16 | uint32(hdr[1])<<8 | uint32(hdr[2])
			state.messageLength = uint32(hdr[3])<<16 | uint32(hdr[4])<<8 | uint32(hdr[5])
			state.messageTypeID = hdr[6]
			state.messageStreamID = uint32(hdr[7]) | uint32(hdr[8])<<8 | uint32(hdr[9])<<16 | uint32(hdr[10])<<24

			if ts == chunkTimestampMax {
				ext, err := decodeExtendedTimestamp(c.br)
				if err != nil {
					return nil, err
				}
				state.timestamp = ext
				state.hasExtended = true
			} else {
				state.timestamp = ts
				state.hasExtended = false
			}
			state.timestampDelta = 0

			if state.messageLength > maxMessageSize {
				return nil, fmt.Errorf("%w: %d bytes", ErrMessageTooLarge, state.messageLength)
			}
			state.payload = make([]byte, 0, state.messageLength)
			state.remaining = state.messageLength

		case 1: // fmt 1 – no stream ID (7 bytes)
			var hdr [7]byte
			if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
				return nil, err
			}
			td := uint32(hdr[0])<<16 | uint32(hdr[1])<<8 | uint32(hdr[2])
			state.messageLength = uint32(hdr[3])<<16 | uint32(hdr[4])<<8 | uint32(hdr[5])
			state.messageTypeID = hdr[6]

			if td == chunkTimestampMax {
				ext, err := decodeExtendedTimestamp(c.br)
				if err != nil {
					return nil, err
				}
				td = ext
				state.hasExtended = true
			} else {
				state.hasExtended = false
			}
			state.timestampDelta = td
			state.timestamp += td

			if state.messageLength > maxMessageSize {
				return nil, fmt.Errorf("%w: %d bytes", ErrMessageTooLarge, state.messageLength)
			}
			state.payload = make([]byte, 0, state.messageLength)
			state.remaining = state.messageLength

		case 2: // fmt 2 – timestamp delta only (3 bytes)
			var hdr [3]byte
			if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
				return nil, err
			}
			td := uint32(hdr[0])<<16 | uint32(hdr[1])<<8 | uint32(hdr[2])

			if td == chunkTimestampMax {
				ext, err := decodeExtendedTimestamp(c.br)
				if err != nil {
					return nil, err
				}
				td = ext
				state.hasExtended = true
			} else {
				state.hasExtended = false
			}
			state.timestampDelta = td
			state.timestamp += td

			state.payload = make([]byte, 0, state.messageLength)
			state.remaining = state.messageLength

		case 3: // fmt 3 – no message header
			if state.hasExtended {
				if _, err := decodeExtendedTimestamp(c.br); err != nil {
					return nil, err
				}
			}
			// If the previous message was complete, start a new one with
			// the same parameters and accumulated timestamp delta.
			if state.remaining == 0 {
				state.timestamp += state.timestampDelta
				state.payload = make([]byte, 0, state.messageLength)
				state.remaining = state.messageLength
			}
		}

		// Read chunk payload directly into pre-allocated slice.
		toRead := min(state.remaining, c.readChunkSize)
		if toRead > 0 {
			start := uint32(len(state.payload))
			state.payload = state.payload[:start+toRead]
			if _, err := io.ReadFull(c.br, state.payload[start:]); err != nil {
				return nil, err
			}
			state.remaining -= toRead
		}

		// Send acknowledgement when needed.
		if err := c.checkAck(); err != nil {
			return nil, err
		}

		// Message complete?
		if state.remaining == 0 {
			msg := &rawMessage{
				typeID:    messageTypeID(state.messageTypeID),
				streamID:  messageStreamID(state.messageStreamID),
				timestamp: state.timestamp,
				payload:   state.payload,
			}
			state.payload = nil
			return msg, nil
		}
	}
}

// writeRawMessage writes a full RTMP message, splitting it into chunks.
func (c *Conn) writeRawMessage(csid chunkStreamID, msg *rawMessage) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	payload := msg.payload
	msgLen := uint32(len(payload))
	chunkSize := c.writeChunkSize
	offset := uint32(0)
	first := true

	for {
		n := chunkSize
		if rem := msgLen - offset; n > rem {
			n = rem
		}

		var bh chunkBasicHeader
		bh.chunkStreamID = csid

		if first {
			bh.fmt = 0
			if err := bh.encode(c.bw); err != nil {
				return err
			}
			var hdr [11]byte
			ts := msg.timestamp
			if ts >= chunkTimestampMax {
				hdr[0], hdr[1], hdr[2] = 0xFF, 0xFF, 0xFF
			} else {
				hdr[0] = byte(ts >> 16)
				hdr[1] = byte(ts >> 8)
				hdr[2] = byte(ts)
			}
			hdr[3] = byte(msgLen >> 16)
			hdr[4] = byte(msgLen >> 8)
			hdr[5] = byte(msgLen)
			hdr[6] = byte(msg.typeID)
			hdr[7] = byte(msg.streamID)
			hdr[8] = byte(msg.streamID >> 8)
			hdr[9] = byte(msg.streamID >> 16)
			hdr[10] = byte(msg.streamID >> 24)
			if _, err := c.bw.Write(hdr[:]); err != nil {
				return err
			}
			if ts >= chunkTimestampMax {
				if err := encodeExtendedTimestamp(c.bw, ts); err != nil {
					return err
				}
			}
			first = false
		} else {
			bh.fmt = 3
			if err := bh.encode(c.bw); err != nil {
				return err
			}
			if msg.timestamp >= chunkTimestampMax {
				if err := encodeExtendedTimestamp(c.bw, msg.timestamp); err != nil {
					return err
				}
			}
		}

		if n > 0 {
			if _, err := c.bw.Write(payload[offset : offset+n]); err != nil {
				return err
			}
			offset += n
		}

		if offset >= msgLen {
			break
		}
	}

	return c.bw.Flush()
}

// ---------------------------------------------------------------------------
// Control message handling
// ---------------------------------------------------------------------------

func (c *Conn) handleControlMessage(msg *rawMessage) error {
	r := bytes.NewReader(msg.payload)

	switch msg.typeID {
	case messageTypeSetChunkSize:
		var m messageSetChunkSize
		if err := m.decode(r); err != nil {
			return err
		}
		if m.ChunkSize < 1 {
			return fmt.Errorf("%w: %d", ErrInvalidChunkSize, m.ChunkSize)
		}
		c.readChunkSize = m.ChunkSize

	case messageTypeAbort:
		var m messageAbort
		if err := m.decode(r); err != nil {
			return err
		}
		if state, ok := c.readStates[m.ChunkStreamID]; ok {
			state.payload = nil
			state.remaining = 0
		}

	case messageTypeAck:
		var m messageAck
		if err := m.decode(r); err != nil {
			return err
		}
		c.peerAckedBytes = uint64(m.SequenceNumber)

	case messageTypeUserControl:
		var m messageUserControl
		if err := m.decode(r); err != nil {
			return err
		}
		if m.EventType == userControlPingRequest && len(m.EventData) >= 4 {
			return c.sendUserControl(userControlPingResponse, m.EventData[:4])
		}

	case messageTypeWindowAckSize:
		var m messageWindowAckSize
		if err := m.decode(r); err != nil {
			return err
		}
		c.windowAckSize = m.Size

	case messageTypeSetPeerBandwidth:
		var m messageSetPeerBandwidth
		if err := m.decode(r); err != nil {
			return err
		}
		// The spec asks for a Window Acknowledgement Size reply, but writing
		// from inside the read path would deadlock on synchronous transports.
		// The negotiation layer sends WindowAckSize explicitly instead.
	}

	return nil
}

func isControlMessage(typeID messageTypeID) bool {
	return typeID >= messageTypeSetChunkSize && typeID <= messageTypeSetPeerBandwidth
}

func (c *Conn) checkAck() error {
	if c.windowAckSize == 0 {
		return nil
	}
	bytesRead := c.counter.count.Load()
	if bytesRead-c.lastAcked >= uint64(c.windowAckSize) {
		c.lastAcked = bytesRead
		return c.sendAck(uint32(bytesRead))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Control / command message senders
// ---------------------------------------------------------------------------

func (c *Conn) sendAck(seq uint32) error {
	var buf bytes.Buffer
	_ = (&messageAck{SequenceNumber: seq}).encode(&buf)
	return c.writeRawMessage(csidControl, &rawMessage{typeID: messageTypeAck, payload: buf.Bytes()})
}

func (c *Conn) sendSetChunkSize(size uint32) error {
	var buf bytes.Buffer
	_ = (&messageSetChunkSize{ChunkSize: size}).encode(&buf)
	return c.writeRawMessage(csidControl, &rawMessage{typeID: messageTypeSetChunkSize, payload: buf.Bytes()})
}

func (c *Conn) sendWindowAckSize(size uint32) error {
	var buf bytes.Buffer
	_ = (&messageWindowAckSize{Size: size}).encode(&buf)
	return c.writeRawMessage(csidControl, &rawMessage{typeID: messageTypeWindowAckSize, payload: buf.Bytes()})
}

func (c *Conn) sendSetPeerBandwidth(bandwidth uint32, limitType bandwidthLimitType) error {
	var buf bytes.Buffer
	_ = (&messageSetPeerBandwidth{Bandwidth: bandwidth, LimitType: limitType}).encode(&buf)
	return c.writeRawMessage(csidControl, &rawMessage{typeID: messageTypeSetPeerBandwidth, payload: buf.Bytes()})
}

func (c *Conn) sendUserControl(eventType uint16, data []byte) error {
	var buf bytes.Buffer
	_ = (&messageUserControl{EventType: eventType, EventData: data}).encode(&buf)
	return c.writeRawMessage(csidControl, &rawMessage{typeID: messageTypeUserControl, payload: buf.Bytes()})
}

func (c *Conn) sendStreamBegin(streamID uint32) error {
	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data, streamID)
	return c.sendUserControl(userControlStreamBegin, data)
}

func (c *Conn) sendStreamEOF(streamID uint32) error {
	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data, streamID)
	return c.sendUserControl(userControlStreamEOF, data)
}

func (c *Conn) sendCommand(streamID messageStreamID, name string, txID float64, args ...any) error {
	var buf bytes.Buffer
	enc := amf0.NewEncoder(&buf)
	if err := enc.Encode(name); err != nil {
		return err
	}
	if err := enc.Encode(txID); err != nil {
		return err
	}
	for _, arg := range args {
		if err := enc.Encode(arg); err != nil {
			return err
		}
	}
	return c.writeRawMessage(csidCommand, &rawMessage{
		typeID:   messageTypeAMF0Command,
		streamID: streamID,
		payload:  buf.Bytes(),
	})
}

func (c *Conn) readCommand(msg *rawMessage) (name string, txID float64, args []any, err error) {
	r := bytes.NewReader(msg.payload)
	if msg.typeID == messageTypeAMF3Command && len(msg.payload) > 0 {
		_, _ = r.ReadByte() // skip leading byte
	}
	dec := amf0.NewDecoder(r)

	nameVal, err := dec.Decode()
	if err != nil {
		return "", 0, nil, fmt.Errorf("rtmp: reading command name: %w", err)
	}
	name, _ = nameVal.(string)

	txVal, err := dec.Decode()
	if err != nil {
		return name, 0, nil, fmt.Errorf("rtmp: reading transaction ID: %w", err)
	}
	txID, _ = txVal.(float64)

	for {
		arg, decErr := dec.Decode()
		if errors.Is(decErr, io.EOF) {
			break
		}
		if decErr != nil {
			return name, txID, args, nil // best-effort, return what we have
		}
		args = append(args, arg)
	}
	return name, txID, args, nil
}

// readNextCommand reads messages until an AMF0 or AMF3 command is received,
// handling control messages automatically.
func (c *Conn) readNextCommand() (name string, txID float64, args []any, err error) {
	for {
		msg, err := c.readMessage()
		if err != nil {
			return "", 0, nil, err
		}
		if isControlMessage(msg.typeID) {
			if err := c.handleControlMessage(msg); err != nil {
				return "", 0, nil, err
			}
			continue
		}
		if msg.typeID == messageTypeAMF0Command || msg.typeID == messageTypeAMF3Command {
			return c.readCommand(msg)
		}
	}
}

// ---------------------------------------------------------------------------
// AcceptStream – server-side RTMP negotiation
// ---------------------------------------------------------------------------

// AcceptStream waits for the remote client to negotiate a publish stream.
// It handles the connect → createStream → publish command sequence
// automatically and returns a [MessageReader] that delivers audio, video,
// and metadata frames.
//
// AcceptStream blocks until the negotiation completes or an error occurs.
// The returned [MessageReader] exposes the application name and stream key
// provided by the client.
func (c *Conn) AcceptStream() (*MessageReader, error) {
	return c.AcceptStreamFunc(nil)
}

// AcceptStreamFunc is [Conn.AcceptStream] with a check of the publish: allow
// is called with the application name and stream key before the publish is
// answered. An error from allow answers it with a NetStream.Publish.Denied
// status instead of NetStream.Publish.Start, and AcceptStreamFunc returns
// that error wrapped in [ErrPublishDenied]. A nil allow accepts every
// publish.
func (c *Conn) AcceptStreamFunc(allow func(app, streamKey string) error) (*MessageReader, error) {
	var (
		activeStreamID messageStreamID
		app            string
	)

	for {
		msg, err := c.readMessage()
		if err != nil {
			return nil, err
		}

		if isControlMessage(msg.typeID) {
			if err := c.handleControlMessage(msg); err != nil {
				return nil, err
			}
			continue
		}

		if msg.typeID != messageTypeAMF0Command && msg.typeID != messageTypeAMF3Command {
			continue
		}

		name, txID, args, err := c.readCommand(msg)
		if err != nil {
			return nil, err
		}

		switch name {
		case commandMessageNameConnect:
			if obj, ok := args[0].(map[string]any); ok {
				app, _ = obj["app"].(string)
			}
			if err := c.handleConnect(txID); err != nil {
				return nil, err
			}

		case commandMessageNameReleaseStream, commandMessageNameFCPublish:
			if err := c.sendCommand(0, commandMessageNameResult, txID, nil); err != nil {
				return nil, err
			}

		case commandMessageNameCreateStream:
			activeStreamID = messageStreamID(c.nextStreamID)
			c.nextStreamID++
			if err := c.sendCommand(0, commandMessageNameResult, txID, nil, float64(activeStreamID)); err != nil {
				return nil, err
			}

		case commandMessageNamePublish:
			var pubName string
			if len(args) >= 2 {
				pubName, _ = args[1].(string)
			}
			sid := msg.streamID
			if sid == 0 {
				sid = activeStreamID
			}
			if allow != nil {
				if denied := allow(app, pubName); denied != nil {
					// The reason stays on this side: the stream key may carry
					// a credential, and the description is the client's to show.
					status := map[string]any{
						"level":       "error",
						"code":        "NetStream.Publish.Denied",
						"description": "publishing was refused",
					}
					if err := c.sendCommand(sid, commandMessageNameOnStatus, 0, nil, status); err != nil {
						return nil, err
					}
					return nil, fmt.Errorf("%w: %w", ErrPublishDenied, denied)
				}
			}
			if err := c.sendStreamBegin(uint32(sid)); err != nil {
				return nil, err
			}
			status := map[string]any{
				"level":       "status",
				"code":        "NetStream.Publish.Start",
				"description": pubName + " is now published.",
			}
			if err := c.sendCommand(sid, commandMessageNameOnStatus, 0, nil, status); err != nil {
				return nil, err
			}
			return &MessageReader{conn: c, streamID: sid, app: app, streamKey: pubName}, nil

		case commandMessageNameDeleteStream, commandMessageNameFCUnpublish:
			// Graceful disconnect – acknowledge silently.
		}
	}
}

func (c *Conn) handleConnect(txID float64) error {
	if err := c.sendWindowAckSize(defaultWindowAckSize); err != nil {
		return err
	}
	if err := c.sendSetPeerBandwidth(defaultWindowAckSize, bandwidthLimitDynamic); err != nil {
		return err
	}
	if err := c.sendSetChunkSize(serverChunkSize); err != nil {
		return err
	}
	c.writeChunkSize = serverChunkSize
	if err := c.sendStreamBegin(0); err != nil {
		return err
	}
	props := map[string]any{
		"fmsVer":       "FMS/3,0,1,123",
		"capabilities": 31.0,
	}
	info := map[string]any{
		"level":          "status",
		"code":           "NetConnection.Connect.Success",
		"description":    "Connection succeeded.",
		"objectEncoding": 0.0,
	}
	return c.sendCommand(0, commandMessageNameResult, txID, props, info)
}

// ---------------------------------------------------------------------------
// OpenStream – client-side RTMP negotiation
// ---------------------------------------------------------------------------

// OpenStream performs the client-side RTMP publish handshake (connect →
// createStream → publish) and returns a [MessageWriter] for sending frames.
//
// The app parameter corresponds to the first path segment of the RTMP URL
// (e.g. "live" for rtmp://host/live/key). The streamKey is the publishing
// name that identifies the stream within the application.
func (c *Conn) OpenStream(app, streamKey string) (*MessageWriter, error) {
	// --- connect ---
	connectObj := map[string]any{
		"app":            app,
		"type":           "nonprivate",
		"flashVer":       "FMLE/3.0",
		"tcUrl":          "rtmp://localhost/" + app,
		"fpad":           false,
		"capabilities":   239.0,
		"audioCodecs":    float64(audioCodecFlagAAC | audioCodecFlagMP3),
		"videoCodecs":    float64(videoCodecFlagH264),
		"videoFunction":  1.0,
		"objectEncoding": 0.0,
	}
	if err := c.sendCommand(0, commandMessageNameConnect, 1, connectObj); err != nil {
		return nil, err
	}
	if err := c.waitForResult(1); err != nil {
		return nil, err
	}

	// --- createStream ---
	if err := c.sendCommand(0, commandMessageNameCreateStream, 2, nil); err != nil {
		return nil, err
	}
	streamID, err := c.waitForStreamID(2)
	if err != nil {
		return nil, err
	}

	// --- publish ---
	if err := c.sendCommand(streamID, commandMessageNamePublish, 0, nil, streamKey, "live"); err != nil {
		return nil, err
	}
	if err := c.waitForOnStatus(); err != nil {
		return nil, err
	}

	return &MessageWriter{conn: c, streamID: streamID, app: app, streamKey: streamKey}, nil
}

func (c *Conn) waitForResult(txID float64) error {
	for {
		name, tid, _, err := c.readNextCommand()
		if err != nil {
			return err
		}
		if name == commandMessageNameResult && tid == txID {
			return nil
		}
		if name == commandMessageNameError && tid == txID {
			return fmt.Errorf("%w: transaction %v", ErrServerRejected, txID)
		}
	}
}

func (c *Conn) waitForStreamID(txID float64) (messageStreamID, error) {
	for {
		name, tid, args, err := c.readNextCommand()
		if err != nil {
			return 0, err
		}
		if name == commandMessageNameResult && tid == txID && len(args) >= 2 {
			if sid, ok := args[1].(float64); ok {
				return messageStreamID(sid), nil
			}
		}
		if name == commandMessageNameError && tid == txID {
			return 0, ErrCreateStreamRejected
		}
	}
}

func (c *Conn) waitForOnStatus() error {
	for {
		name, _, _, err := c.readNextCommand()
		if err != nil {
			return err
		}
		if name == commandMessageNameOnStatus {
			return nil
		}
	}
}

// ---------------------------------------------------------------------------
// Connection management
// ---------------------------------------------------------------------------

// LocalAddr returns the local network address of the underlying TCP connection.
func (c *Conn) LocalAddr() net.Addr { return c.transport.LocalAddr() }

// RemoteAddr returns the remote network address of the underlying TCP connection.
func (c *Conn) RemoteAddr() net.Addr { return c.transport.RemoteAddr() }

// Close closes the underlying TCP connection.
func (c *Conn) Close() error { return c.transport.Close() }

// ---------------------------------------------------------------------------
// MessageReader / MessageWriter
// ---------------------------------------------------------------------------

// MessageReader delivers audio, video and metadata frames from a publish
// stream accepted via [Conn.AcceptStream].
type MessageReader struct {
	conn      *Conn
	streamID  messageStreamID
	app       string
	streamKey string
}

// App returns the RTMP application name supplied by the remote client
// during the connect handshake (the first path element of the RTMP URL).
func (r *MessageReader) App() string { return r.app }

// StreamKey returns the publishing name supplied by the remote client
// in the publish command (the stream key portion of the RTMP URL).
func (r *MessageReader) StreamKey() string { return r.streamKey }

// Close sends a Stream EOF user control event to the remote client and
// closes the underlying connection.
func (r *MessageReader) Close() error {
	_ = r.conn.sendStreamEOF(uint32(r.streamID))
	return r.conn.Close()
}

// ReadFrame reads the next audio, video, or metadata frame from the stream.
// It blocks until a frame is available or an error occurs. Control messages
// are handled transparently. When the remote client disconnects, ReadFrame
// returns an error wrapping [io.EOF].
func (r *MessageReader) ReadFrame() (*Frame, error) {
	for {
		msg, err := r.conn.readMessage()
		if err != nil {
			return nil, err
		}

		if isControlMessage(msg.typeID) {
			if err := r.conn.handleControlMessage(msg); err != nil {
				return nil, err
			}
			continue
		}

		// Skip messages destined for other streams.
		if msg.streamID != r.streamID {
			continue
		}

		switch msg.typeID {
		case messageTypeAudio:
			return &Frame{Type: FrameTypeAudio, Timestamp: msg.timestamp, Data: msg.payload}, nil
		case messageTypeVideo:
			return &Frame{Type: FrameTypeVideo, Timestamp: msg.timestamp, Data: msg.payload}, nil
		case messageTypeAMF0Data, messageTypeAMF3Data:
			return &Frame{Type: FrameTypeMetadata, Timestamp: msg.timestamp, Data: msg.payload}, nil
		}
	}
}

// MessageWriter sends audio, video and metadata frames on a publish stream
// opened via [Conn.OpenStream].
type MessageWriter struct {
	conn      *Conn
	streamID  messageStreamID
	app       string
	streamKey string
}

// App returns the RTMP application name used when opening the stream.
func (w *MessageWriter) App() string { return w.app }

// StreamKey returns the stream key used when opening the stream.
func (w *MessageWriter) StreamKey() string { return w.streamKey }

// Close sends a deleteStream command to the remote server and closes
// the underlying connection.
func (w *MessageWriter) Close() error {
	// Best-effort: try to send deleteStream before closing.
	_ = w.conn.sendCommand(0, commandMessageNameDeleteStream, 0, nil, float64(w.streamID))
	return w.conn.Close()
}

// WriteFrame writes a single audio, video, or metadata frame to the stream.
// The frame's [Frame.Type] determines which RTMP chunk stream is used.
func (w *MessageWriter) WriteFrame(frame *Frame) error {
	var typeID messageTypeID
	var csid chunkStreamID

	switch frame.Type {
	case FrameTypeAudio:
		typeID = messageTypeAudio
		csid = csidAudio
	case FrameTypeVideo:
		typeID = messageTypeVideo
		csid = csidVideo
	case FrameTypeMetadata:
		typeID = messageTypeAMF0Data
		csid = csidCommand
	default:
		return fmt.Errorf("%w: %d", ErrUnsupportedFrameType, frame.Type)
	}

	return w.conn.writeRawMessage(csid, &rawMessage{
		typeID:    typeID,
		streamID:  w.streamID,
		timestamp: frame.Timestamp,
		payload:   frame.Data,
	})
}
