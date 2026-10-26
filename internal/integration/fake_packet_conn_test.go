//go:build integration

package integration

import (
	"net"
	"sync/atomic"
)

// low-level utility: the network change under test happens at the client's
// UDP socket, below any gomoqt or relay interface, so the fakes sit at
// net.PacketConn. Each wraps a real socket, which it embeds as the
// net.PacketConn interface, not *net.UDPConn, so quic-go cannot reach the
// socket's ReadMsgUDP/WriteMsgUDP past the fake.

var (
	_ net.PacketConn = (*fakeDroppableConn)(nil)
	_ net.PacketConn = (*fakeCountingConn)(nil)
)

// fakeDroppableConn is a UDP socket whose link can be cut: once down is set,
// it drops every datagram it sends or receives, as a lost Wi-Fi network does.
// Writes report success either way.
type fakeDroppableConn struct {
	net.PacketConn
	down atomic.Bool
}

func (c *fakeDroppableConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(p)
		if err != nil || !c.down.Load() {
			return n, addr, err
		}
	}
}

func (c *fakeDroppableConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if c.down.Load() {
		return len(p), nil
	}
	return c.PacketConn.WriteTo(p, addr)
}

// fakeCountingConn is a UDP socket that counts the datagrams it reads, to
// show which path the relay sends over.
type fakeCountingConn struct {
	net.PacketConn
	reads atomic.Int64
}

func (c *fakeCountingConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(p)
	if err == nil {
		c.reads.Add(1)
	}
	return n, addr, err
}
