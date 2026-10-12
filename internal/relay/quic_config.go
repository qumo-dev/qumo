package relay

import (
	"time"

	"github.com/quic-go/quic-go"

	"github.com/qumo-dev/qumo/internal/quicconfig"
)

const (
	// defaultQUICKeepAlive and defaultQUICMaxIdleTimeout are the relay's own
	// defaults: a keep-alive well inside the idle timeout, so a quiet but
	// healthy session stays open.
	defaultQUICKeepAlive      = 10 * time.Second
	defaultQUICMaxIdleTimeout = 60 * time.Second
)

// quicConfigFromEnv builds the QUIC configuration of the relay's listener and
// of its peer dialer: the relay's own defaults, with the QUIC_* settings
// found in the environment applied (quicconfig.FromEnv lists and checks
// them). Unset, the keep-alive period is 10s and the idle timeout 60s.
//
// WebSocket sessions take part of this configuration too (gomoqt's qmux): the
// two timeouts, the two initial windows, and the keep-alive period. Zero
// sends no pings there either, and then a proxy may close an idle session.
// The packet size, path MTU discovery and the max windows are QUIC only.
//
// The stream limits (MaxIncomingStreams, MaxIncomingUniStreams) are not
// settings and stay at quic-go's defaults (100). gomoqt's OpenGroup blocks on
// MAX_STREAMS as backpressure, and the relay gives it a context with a
// deadline (openGroupTimeout), so a blocked open drops the group rather than
// hanging the egress goroutine. Raising the limits removes that backpressure:
// stream objects are then retained without bound and the garbage collector
// falls behind.
//
// 0-RTT, datagrams and partial delivery on reset are what MoQ sessions need,
// so they are always on.
func quicConfigFromEnv() (*quic.Config, error) {
	return quicconfig.FromEnv(&quic.Config{
		Allow0RTT:                        true,
		EnableDatagrams:                  true,
		EnableStreamResetPartialDelivery: true,
		KeepAlivePeriod:                  defaultQUICKeepAlive,
		MaxIdleTimeout:                   defaultQUICMaxIdleTimeout,
	})
}
