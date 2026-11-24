package quicconfig

// The names of the QUIC transport settings. They carry no command's prefix,
// so every command with a QUIC listener or dialer can read the same ones.
const (
	// KeyInitialPacketSize is the UDP payload, in bytes, a connection starts
	// with.
	KeyInitialPacketSize = "QUIC_INITIAL_PACKET_SIZE"
	// KeyPMTUD is the switch for path MTU discovery.
	KeyPMTUD = "QUIC_PMTUD"
	// KeyKeepAlive is the keep-alive period, a duration.
	KeyKeepAlive = "QUIC_KEEPALIVE"
	// KeyIdleTimeout is how long a session may stay silent, a duration.
	KeyIdleTimeout = "QUIC_IDLE_TIMEOUT"
	// KeyHandshakeIdleTimeout is how long a handshake may stay silent, a
	// duration.
	KeyHandshakeIdleTimeout = "QUIC_HANDSHAKE_IDLE_TIMEOUT"
	// KeyStreamReceiveWindow is the receive window a stream starts with, in
	// bytes.
	KeyStreamReceiveWindow = "QUIC_STREAM_RECEIVE_WINDOW"
	// KeyMaxStreamReceiveWindow is the largest a stream's receive window grows
	// to, in bytes.
	KeyMaxStreamReceiveWindow = "QUIC_MAX_STREAM_RECEIVE_WINDOW"
	// KeyConnectionReceiveWindow is the receive window a connection starts with,
	// in bytes.
	KeyConnectionReceiveWindow = "QUIC_CONNECTION_RECEIVE_WINDOW"
	// KeyMaxConnectionReceiveWindow is the largest a connection's receive window
	// grows to, in bytes.
	KeyMaxConnectionReceiveWindow = "QUIC_MAX_CONNECTION_RECEIVE_WINDOW"
)

// Keys lists every QUIC transport setting, in the order they are documented.
func Keys() []string {
	return []string{
		KeyInitialPacketSize,
		KeyPMTUD,
		KeyKeepAlive,
		KeyIdleTimeout,
		KeyHandshakeIdleTimeout,
		KeyStreamReceiveWindow,
		KeyMaxStreamReceiveWindow,
		KeyConnectionReceiveWindow,
		KeyMaxConnectionReceiveWindow,
	}
}
