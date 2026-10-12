package envconfig

// The names of the QUIC transport settings. They carry no command's prefix,
// so every command with a QUIC listener or dialer can read the same ones.
const (
	// QUICInitialPacketSize is the UDP payload, in bytes, a connection starts
	// with.
	QUICInitialPacketSize = "QUIC_INITIAL_PACKET_SIZE"
	// QUICPMTUD is the switch for path MTU discovery.
	QUICPMTUD = "QUIC_PMTUD"
	// QUICKeepAlive is the keep-alive period, a duration.
	QUICKeepAlive = "QUIC_KEEPALIVE"
	// QUICIdleTimeout is how long a session may stay silent, a duration.
	QUICIdleTimeout = "QUIC_IDLE_TIMEOUT"
	// QUICHandshakeIdleTimeout is how long a handshake may stay silent, a
	// duration.
	QUICHandshakeIdleTimeout = "QUIC_HANDSHAKE_IDLE_TIMEOUT"
	// QUICStreamReceiveWindow is the receive window a stream starts with, in
	// bytes.
	QUICStreamReceiveWindow = "QUIC_STREAM_RECEIVE_WINDOW"
	// QUICMaxStreamReceiveWindow is the largest a stream's receive window
	// grows to, in bytes.
	QUICMaxStreamReceiveWindow = "QUIC_MAX_STREAM_RECEIVE_WINDOW"
	// QUICConnectionReceiveWindow is the receive window a connection starts
	// with, in bytes.
	QUICConnectionReceiveWindow = "QUIC_CONNECTION_RECEIVE_WINDOW"
	// QUICMaxConnectionReceiveWindow is the largest a connection's receive
	// window grows to, in bytes.
	QUICMaxConnectionReceiveWindow = "QUIC_MAX_CONNECTION_RECEIVE_WINDOW"
)

// QUICKeys lists every QUIC transport setting, in the order they are
// documented.
func QUICKeys() []string {
	return []string{
		QUICInitialPacketSize,
		QUICPMTUD,
		QUICKeepAlive,
		QUICIdleTimeout,
		QUICHandshakeIdleTimeout,
		QUICStreamReceiveWindow,
		QUICMaxStreamReceiveWindow,
		QUICConnectionReceiveWindow,
		QUICMaxConnectionReceiveWindow,
	}
}
