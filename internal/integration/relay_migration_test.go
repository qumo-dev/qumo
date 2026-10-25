//go:build integration

// Black-box tests of QUIC connection migration (RFC 9000 §9) against a real
// relay: a subscriber whose network changes mid-session, as a phone moving
// from Wi-Fi to cellular does, keeps receiving its subscription over the same
// QUIC connection and MOQ session, without a new handshake.
//
// The relay does nothing for this itself: quic-go validates the new path and
// moves the connection to it. These tests pin that behaviour so a relay or
// quic-go change that breaks it fails here.
package integration

import (
	"context"
	"crypto/tls"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/gomoqt/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const migrationPath = moqt.BroadcastPath("/mobile/live")

func TestRelay_ConnectionMigration(t *testing.T) {
	addr, srv := startAuthRelay(t, authOff, nil)
	publishOver(t, srv, "https://"+addr+"/", migrationPath)
	sub := dialMigratable(t, addr)
	tr := subscribeMigration(t, sub.sess)
	last := nextGroup(t, tr, 5*time.Second)

	// The phone joins cellular: probe a path from a new socket, then move the
	// connection onto it.
	cellular := listenUDP(t)
	path, err := sub.conn.AddPath(&quic.Transport{Conn: cellular})
	require.NoError(t, err)
	probeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, path.Probe(probeCtx), "the relay should answer the PATH_CHALLENGE")
	require.NoError(t, path.Switch())
	// Wi-Fi is gone: nothing more is sent or received over the old socket,
	// so every later group must arrive over cellular.
	sub.wifi.down.Store(true)

	for range 5 {
		last = nextGroupAfter(t, tr, last, 5*time.Second)
	}

	assert.NoError(t, sub.sess.Context().Err(), "the MOQ session should survive the move")
	assert.NoError(t, sub.conn.Context().Err(), "the QUIC connection should survive the move")
}

// Without a switch, losing Wi-Fi stops the subscription: this proves the
// dropped socket really cuts the old path, so the test above passes only
// because the connection moved.
func TestRelay_ConnectionMigration_LinkLostWithoutSwitch(t *testing.T) {
	addr, srv := startAuthRelay(t, authOff, nil)
	publishOver(t, srv, "https://"+addr+"/", migrationPath)
	sub := dialMigratable(t, addr)
	tr := subscribeMigration(t, sub.sess)
	last := nextGroup(t, tr, 5*time.Second)

	sub.wifi.down.Store(true)

	// Groups already in flight may still be delivered from the client's
	// buffers; after them, nothing new should arrive.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		gr, err := tr.AcceptGroup(ctx)
		if err != nil {
			break
		}
		assert.Less(t, gr.GroupSequence(), last+5, "groups kept arriving over a dead path")
	}
}

// migratableSub is a native-QUIC subscriber session whose QUIC connection
// the test holds, so it can move it to a new path.
type migratableSub struct {
	sess *moqt.Session
	conn *quic.Conn
	wifi *droppableConn
}

// dialMigratable dials the relay at addr over native QUIC from a socket the
// test can cut, through a quic.Transport the test owns: quic-go's AddPath
// needs the connection, which gomoqt's default dialer keeps to itself.
func dialMigratable(t *testing.T, addr string) migratableSub {
	t.Helper()
	sub := migratableSub{wifi: &droppableConn{PacketConn: listenUDP(t)}}
	wifi := &quic.Transport{Conn: sub.wifi}
	t.Cleanup(func() { _ = wifi.Close() })
	dialer := &moqt.Dialer{
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // test-only self-signed cert
			MinVersion:         tls.VersionTLS13,
		},
		QUICConfig: &quic.Config{KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second},
		DialQUICFunc: func(ctx context.Context, addr string, tlsConf *tls.Config, quicConf *quic.Config) (moqt.StreamConn, error) {
			raddr, err := net.ResolveUDPAddr("udp", addr)
			if err != nil {
				return nil, err
			}
			conn, err := wifi.DialEarly(ctx, raddr, tlsConf, quicConf)
			if err != nil {
				return nil, err
			}
			sub.conn = conn
			return streamConn{conn}, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := dialer.Dial(ctx, nativeURL(addr)+"/", moqt.NewTrackMux(0))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.CloseWithError(moqt.NoError, "test done") })
	sub.sess = sess
	return sub
}

func subscribeMigration(t *testing.T, sess *moqt.Session) *moqt.TrackReader {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, err := sess.Subscribe(ctx, migrationPath, testTrack, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

// nextGroup returns the sequence of the next group on tr.
func nextGroup(t *testing.T, tr *moqt.TrackReader, timeout time.Duration) moqt.GroupSequence {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	gr, err := tr.AcceptGroup(ctx)
	require.NoError(t, err)
	return gr.GroupSequence()
}

// nextGroupAfter returns the sequence of the first group on tr later than
// last, skipping any older one still in flight.
func nextGroupAfter(t *testing.T, tr *moqt.TrackReader, last moqt.GroupSequence, timeout time.Duration) moqt.GroupSequence {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		gr, err := tr.AcceptGroup(ctx)
		require.NoError(t, err, "no group after %d", last)
		if seq := gr.GroupSequence(); seq > last {
			return seq
		}
	}
}

func listenUDP(t *testing.T) net.PacketConn {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// droppableConn is a UDP socket whose link can be cut: once down, it drops
// every datagram it sends or receives, as a lost Wi-Fi network does.
//
// low-level utility: the network change under test happens at the socket,
// below any gomoqt or relay interface.
//
// It embeds the net.PacketConn interface, not *net.UDPConn, so quic-go cannot
// reach the socket's ReadMsgUDP/WriteMsgUDP past the drop.
type droppableConn struct {
	net.PacketConn
	down atomic.Bool
}

func (c *droppableConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(p)
		if err != nil || !c.down.Load() {
			return n, addr, err
		}
	}
}

func (c *droppableConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if c.down.Load() {
		return len(p), nil
	}
	return c.PacketConn.WriteTo(p, addr)
}

// streamConn adapts a quic-go connection to gomoqt's transport: gomoqt's
// stream and error-code types are quic-go's, so each method only widens the
// returned stream to the interface.
type streamConn struct{ *quic.Conn }

var _ transport.StreamConn = streamConn{}

func (c streamConn) AcceptStream(ctx context.Context) (transport.Stream, error) {
	s, err := c.Conn.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (c streamConn) AcceptUniStream(ctx context.Context) (transport.ReceiveStream, error) {
	s, err := c.Conn.AcceptUniStream(ctx)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (c streamConn) OpenStream() (transport.Stream, error) {
	s, err := c.Conn.OpenStream()
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (c streamConn) OpenStreamSync(ctx context.Context) (transport.Stream, error) {
	s, err := c.Conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (c streamConn) OpenUniStream() (transport.SendStream, error) {
	s, err := c.Conn.OpenUniStream()
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (c streamConn) OpenUniStreamSync(ctx context.Context) (transport.SendStream, error) {
	s, err := c.Conn.OpenUniStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (c streamConn) TLS() *tls.ConnectionState {
	state := c.ConnectionState().TLS
	return &state
}
