package relay

import (
	"errors"
	"net"
	"sync"
	"time"
)

// tlsRecordHandshake is the first byte of a TLS connection: the content
// type of the record that carries the ClientHello. No HTTP request starts
// with it.
const tlsRecordHandshake = 0x16

// sniffTimeout is how long a new connection has to send its first byte.
const sniffTimeout = 5 * time.Second

// splitTLS splits the connections of ln into two listeners by their first
// byte: those that start a TLS handshake, and the rest. It lets one TCP
// port serve HTTPS next to the plain HTTP it has always served, so that
// WebSocket clients reach the relay at the port they know it by. Closing
// either listener closes ln, and with it the other.
func splitTLS(ln net.Listener) (tlsListener, plainListener net.Listener) {
	s := &tlsSplitter{
		ln:     ln,
		tls:    make(chan net.Conn),
		plain:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
	// Ends when ln is closed, which either listener's Close does.
	go s.accept()
	return &splitListener{splitter: s, conns: s.tls}, &splitListener{splitter: s, conns: s.plain}
}

type tlsSplitter struct {
	ln    net.Listener
	tls   chan net.Conn
	plain chan net.Conn

	closeOnce sync.Once
	closed    chan struct{}
}

func (s *tlsSplitter) close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.closed)
		err = s.ln.Close()
	})
	return err
}

// accept hands each new connection to route, until the listener is closed.
// Another accept error is passed over after a pause, as net/http does: it
// is the host running out of something, and passes.
func (s *tlsSplitter) accept() {
	const maxPause = time.Second
	pause := 5 * time.Millisecond
	for {
		conn, err := s.ln.Accept()
		if err == nil {
			pause = 5 * time.Millisecond
			// Ends once the connection has sent a byte, or after
			// sniffTimeout.
			go s.route(conn)
			continue
		}
		if errors.Is(err, net.ErrClosed) {
			_ = s.close() // not actionable: the listener is closed already
			return
		}
		timer := time.NewTimer(pause)
		select {
		case <-timer.C:
			pause = min(2*pause, maxPause)
		case <-s.closed:
			timer.Stop()
			return
		}
	}
}

// route reads the first byte of conn and hands the connection, with the
// byte still to be read, to the listener it belongs to.
func (s *tlsSplitter) route(conn net.Conn) {
	first := make([]byte, 1)
	_ = conn.SetReadDeadline(time.Now().Add(sniffTimeout)) // not actionable: the read below fails instead
	if _, err := conn.Read(first); err != nil {
		_ = conn.Close() // not actionable: the connection sent nothing
		return
	}
	_ = conn.SetReadDeadline(time.Time{}) // not actionable: the server sets its own deadlines

	to := s.plain
	if first[0] == tlsRecordHandshake {
		to = s.tls
	}
	select {
	case to <- &prefixedConn{Conn: conn, prefix: first}:
	case <-s.closed:
		_ = conn.Close() // not actionable: the listener is closed
	}
}

// splitListener is one side of a tlsSplitter.
type splitListener struct {
	splitter *tlsSplitter
	conns    chan net.Conn
}

var _ net.Listener = (*splitListener)(nil)

func (l *splitListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.splitter.closed:
		return nil, net.ErrClosed
	}
}

func (l *splitListener) Close() error   { return l.splitter.close() }
func (l *splitListener) Addr() net.Addr { return l.splitter.ln.Addr() }

// prefixedConn is a connection whose first bytes were read already: Read
// returns them before the rest.
type prefixedConn struct {
	net.Conn
	prefix []byte
}

func (c *prefixedConn) Read(p []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}
