package rtsp

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Conn represents an RTSP connection.
type Conn struct {
	transport net.Conn
	br        *bufio.Reader
	bw        *bufio.Writer

	mu sync.Mutex
}

func newConn(transport net.Conn) *Conn {
	return NewConn(transport)
}

// NewConn wraps an already-established network connection as an RTSP [Conn].
// It is the client-side entry point: the server side constructs Conns via
// [Listener.Accept]. The returned Conn can [Conn.WriteRequest] /
// [Conn.ReadResponse] / [Conn.ReadRequest] (interleaved frames) symmetrically.
func NewConn(transport net.Conn) *Conn {
	return &Conn{
		transport: transport,
		br:        bufio.NewReader(transport),
		bw:        bufio.NewWriter(transport),
	}
}

// ReadRequest reads an RTSP request or an interleaved frame.
func (c *Conn) ReadRequest() (*Request, *InterleavedFrame, error) {
	b, err := c.br.Peek(1)
	if err != nil {
		return nil, nil, err
	}

	if b[0] == '$' {
		frame, err := c.readInterleavedFrame()
		return nil, frame, err
	}

	req, err := ReadRequest(c.br)
	return req, nil, err
}

// ReadResponse reads an RTSP response or an interleaved frame.
func (c *Conn) ReadResponse(req *Request) (*Response, *InterleavedFrame, error) {
	b, err := c.br.Peek(1)
	if err != nil {
		return nil, nil, err
	}

	if b[0] == '$' {
		frame, err := c.readInterleavedFrame()
		return nil, frame, err
	}

	resp, err := ReadResponse(c.br, req)
	return resp, nil, err
}

// responsePrefix is how the first line of every RTSP response begins. A
// request begins with its method instead.
const responsePrefix = "RTSP/"

// readStreamed reads the next thing a server sends while it is streaming: an
// interleaved frame, or an RTSP message on the control channel.
//
// A message is read whole, body included, so that the next read starts at the
// next message. A response is returned; a request from the server is read and
// dropped, and both results are then nil.
func (c *Conn) readStreamed() (*InterleavedFrame, *Response, error) {
	b, err := c.br.Peek(1)
	if err != nil {
		return nil, nil, err
	}
	if b[0] == '$' {
		frame, err := c.readInterleavedFrame()
		return frame, nil, err
	}

	// Every message is longer than the prefix, so waiting for that many
	// bytes cannot wait for more than the message itself.
	start, err := c.br.Peek(len(responsePrefix))
	if err != nil {
		return nil, nil, err
	}
	if string(start) != responsePrefix {
		req, err := ReadRequest(c.br)
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, discardBody(req.Body)
	}

	resp, err := ReadResponse(c.br, nil)
	if err != nil {
		return nil, nil, err
	}
	if err := discardBody(resp.Body); err != nil {
		return nil, nil, err
	}
	resp.Body = nil
	return nil, resp, nil
}

// discardBody reads a message body to its end. The body is a window onto the
// connection's reader: left unread, its bytes would be taken for the start of
// the next message.
func discardBody(body io.Reader) error {
	if body == nil {
		return nil
	}
	_, err := io.Copy(io.Discard, body)
	return err
}

// writeRequestBy writes a request, giving up at deadline. A peer that has
// stopped reading would otherwise hold the write, and with it the lock every
// other write on the connection needs.
func (c *Conn) writeRequestBy(req *Request, deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.transport.SetWriteDeadline(deadline); err != nil {
		return err
	}
	// not actionable: the deadline is cleared so that later writes are not
	// cut short; a connection that cannot clear it fails those writes anyway.
	defer func() { _ = c.transport.SetWriteDeadline(time.Time{}) }()

	if err := req.Write(c.bw); err != nil {
		return err
	}
	return c.bw.Flush()
}

func (c *Conn) readInterleavedFrame() (*InterleavedFrame, error) {
	var header [4]byte
	if _, err := io.ReadFull(c.br, header[:]); err != nil {
		return nil, err
	}

	if header[0] != '$' {
		return nil, fmt.Errorf("malformed interleaved frame header")
	}

	channel := header[1]
	length := binary.BigEndian.Uint16(header[2:])

	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return nil, err
	}

	return &InterleavedFrame{
		Channel: channel,
		Payload: payload,
	}, nil
}

// WriteRequest writes an RTSP request to the connection.
func (c *Conn) WriteRequest(req *Request) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := req.Write(c.bw); err != nil {
		return err
	}
	return c.bw.Flush()
}

// WriteResponse writes an RTSP response to the connection.
func (c *Conn) WriteResponse(resp *Response) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := resp.Write(c.bw); err != nil {
		return err
	}
	return c.bw.Flush()
}

// WriteInterleavedFrame writes an interleaved frame to the connection.
func (c *Conn) WriteInterleavedFrame(frame *InterleavedFrame) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var header [4]byte
	header[0] = '$'
	header[1] = frame.Channel
	binary.BigEndian.PutUint16(header[2:], uint16(len(frame.Payload)))

	if _, err := c.bw.Write(header[:]); err != nil {
		return err
	}
	if _, err := c.bw.Write(frame.Payload); err != nil {
		return err
	}
	return c.bw.Flush()
}

// Close closes the connection.
func (c *Conn) Close() error {
	return c.transport.Close()
}

// RemoteAddr returns the remote network address.
func (c *Conn) RemoteAddr() net.Addr {
	return c.transport.RemoteAddr()
}
