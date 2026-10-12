package relay

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/quicvarint"

	"github.com/qumo-dev/qumo/internal/envconfig"
)

const (
	// defaultQUICKeepAlive and defaultQUICMaxIdleTimeout are the relay's own
	// defaults: a keep-alive well inside the idle timeout, so a quiet but
	// healthy session stays open.
	defaultQUICKeepAlive      = 10 * time.Second
	defaultQUICMaxIdleTimeout = 60 * time.Second

	// minQUICInitialPacketSize is the smallest packet QUIC allows a
	// connection to start with (RFC 9000 §14.1), and maxQUICInitialPacketSize
	// the largest quic-go sends. quic-go clamps a value outside them without
	// saying so; the relay refuses it instead.
	minQUICInitialPacketSize = 1200
	maxQUICInitialPacketSize = 1452

	// minQUICTimeout is the shortest timeout the relay takes. A peer is told
	// the idle timeout in whole milliseconds, where zero means none, so a
	// shorter one would be announced as no timeout at all.
	minQUICTimeout = time.Millisecond

	// quic-go's receive windows when a setting is left unset (quic-go
	// v0.63.0, internal/protocol): what an unset half of an initial/max pair
	// is checked against.
	quicGoInitialStreamWindow     = 512 << 10
	quicGoMaxStreamWindow         = 6 << 20
	quicGoInitialConnectionWindow = 768 << 10
	quicGoMaxConnectionWindow     = 15 << 20
)

// The RELAY_QUIC_* settings, one variable each.
const (
	envQUICInitialPacketSize       = "RELAY_QUIC_INITIAL_PACKET_SIZE"
	envQUICDisablePMTUD            = "RELAY_QUIC_DISABLE_PMTUD"
	envQUICKeepAlivePeriod         = "RELAY_QUIC_KEEPALIVE_PERIOD"
	envQUICMaxIdleTimeout          = "RELAY_QUIC_MAX_IDLE_TIMEOUT"
	envQUICHandshakeIdleTimeout    = "RELAY_QUIC_HANDSHAKE_IDLE_TIMEOUT"
	envQUICInitialStreamWindow     = "RELAY_QUIC_INITIAL_STREAM_RECEIVE_WINDOW"
	envQUICMaxStreamWindow         = "RELAY_QUIC_MAX_STREAM_RECEIVE_WINDOW"
	envQUICInitialConnectionWindow = "RELAY_QUIC_INITIAL_CONNECTION_RECEIVE_WINDOW"
	envQUICMaxConnectionWindow     = "RELAY_QUIC_MAX_CONNECTION_RECEIVE_WINDOW"
)

// quicEnvKeys lists every RELAY_QUIC_* setting, in the order they are
// documented.
func quicEnvKeys() []string {
	return []string{
		envQUICInitialPacketSize,
		envQUICDisablePMTUD,
		envQUICKeepAlivePeriod,
		envQUICMaxIdleTimeout,
		envQUICHandshakeIdleTimeout,
		envQUICInitialStreamWindow,
		envQUICMaxStreamWindow,
		envQUICInitialConnectionWindow,
		envQUICMaxConnectionWindow,
	}
}

// quicEnvSet returns the RELAY_QUIC_* settings present in the environment as
// "NAME=value", for the startup log: a setting that never reached the process
// fails the same way as one left unset.
func quicEnvSet() []string {
	var set []string
	for _, key := range quicEnvKeys() {
		if v := os.Getenv(key); v != "" {
			set = append(set, key+"="+sanitizeLog(v))
		}
	}
	return set
}

// quicConfigFromEnv builds the QUIC configuration of the relay's listener and
// of its peer dialer from the environment. Unset, a setting keeps the relay's
// default or, where the relay has none, quic-go's. A value quic-go would
// reinterpret or clamp without saying so is refused.
//
//	RELAY_QUIC_INITIAL_PACKET_SIZE               - UDP payload bytes a connection starts with, 1200 to 1452 (quic-go: 1280)
//	RELAY_QUIC_DISABLE_PMTUD                     - a switch; on keeps packets at the initial size (default: off)
//	RELAY_QUIC_KEEPALIVE_PERIOD                  - keep-alive interval, e.g. "10s"; "0" sends none (default: 10s)
//	RELAY_QUIC_MAX_IDLE_TIMEOUT                  - close a session silent for this long, 1ms or more (default: 60s)
//	RELAY_QUIC_HANDSHAKE_IDLE_TIMEOUT            - give up a handshake silent for this long, 1ms or more (quic-go: 5s)
//	RELAY_QUIC_INITIAL_STREAM_RECEIVE_WINDOW     - bytes (quic-go: 512 KB)
//	RELAY_QUIC_MAX_STREAM_RECEIVE_WINDOW         - bytes, no less than the initial one (quic-go: 6 MB)
//	RELAY_QUIC_INITIAL_CONNECTION_RECEIVE_WINDOW - bytes (quic-go: 768 KB)
//	RELAY_QUIC_MAX_CONNECTION_RECEIVE_WINDOW     - bytes, no less than the initial one (quic-go: 15 MB)
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
	cfg := &quic.Config{
		Allow0RTT:                        true,
		EnableDatagrams:                  true,
		EnableStreamResetPartialDelivery: true,
		KeepAlivePeriod:                  defaultQUICKeepAlive,
		MaxIdleTimeout:                   defaultQUICMaxIdleTimeout,
	}

	size, set, err := envconfig.Uint(envQUICInitialPacketSize)
	if err != nil {
		return nil, err
	}
	if set {
		if size < minQUICInitialPacketSize || size > maxQUICInitialPacketSize {
			return nil, fmt.Errorf("invalid %s %d: want %d to %d",
				envQUICInitialPacketSize, size, minQUICInitialPacketSize, maxQUICInitialPacketSize)
		}
		cfg.InitialPacketSize = uint16(size)
	}

	if cfg.DisablePathMTUDiscovery, err = envSwitch(envQUICDisablePMTUD, false); err != nil {
		return nil, err
	}

	keepAlive, set, err := envconfig.Duration(envQUICKeepAlivePeriod)
	if err != nil {
		return nil, err
	}
	if set {
		cfg.KeepAlivePeriod = keepAlive
	}
	if err := setQUICTimeout(envQUICMaxIdleTimeout, &cfg.MaxIdleTimeout); err != nil {
		return nil, err
	}
	if err := setQUICTimeout(envQUICHandshakeIdleTimeout, &cfg.HandshakeIdleTimeout); err != nil {
		return nil, err
	}

	if cfg.InitialStreamReceiveWindow, cfg.MaxStreamReceiveWindow, err = quicWindows(
		envQUICInitialStreamWindow, envQUICMaxStreamWindow,
		quicGoInitialStreamWindow, quicGoMaxStreamWindow,
	); err != nil {
		return nil, err
	}
	if cfg.InitialConnectionReceiveWindow, cfg.MaxConnectionReceiveWindow, err = quicWindows(
		envQUICInitialConnectionWindow, envQUICMaxConnectionWindow,
		quicGoInitialConnectionWindow, quicGoMaxConnectionWindow,
	); err != nil {
		return nil, err
	}
	return cfg, nil
}

// setQUICTimeout reads key into dst, leaving dst alone when key is unset. It
// refuses zero, which quic-go reads as "use my default", not "no timeout",
// and anything under minQUICTimeout.
func setQUICTimeout(key string, dst *time.Duration) error {
	v, set, err := envconfig.Duration(key)
	if err != nil {
		return err
	}
	if !set {
		return nil
	}
	if v < minQUICTimeout {
		return fmt.Errorf("invalid %s %s: want %s or more; unset it for the default", key, v, minQUICTimeout)
	}
	*dst = v
	return nil
}

// quicWindows reads an initial/max pair of receive windows. Each is zero when
// unset, which leaves it to quic-go, whose defaults are defInitial and
// defMax. It refuses a window larger than a QUIC varint holds, which quic-go
// would panic on at the first handshake, and an initial window above the max
// one in effect, which quic-go would then ignore.
func quicWindows(initialKey, maxKey string, defInitial, defMax uint64) (initial, maxWindow uint64, err error) {
	if initial, err = quicWindow(initialKey); err != nil {
		return 0, 0, err
	}
	if maxWindow, err = quicWindow(maxKey); err != nil {
		return 0, 0, err
	}
	effInitial, effMax := initial, maxWindow
	if effInitial == 0 {
		effInitial = defInitial
	}
	if effMax == 0 {
		effMax = defMax
	}
	if effInitial > effMax {
		return 0, 0, fmt.Errorf("invalid %s %d: less than the initial window %d (%s); the window starts at the initial size and never shrinks",
			maxKey, effMax, effInitial, initialKey)
	}
	return initial, maxWindow, nil
}

// quicWindow reads one receive window: zero when unset, otherwise 1 to the
// largest QUIC varint.
func quicWindow(key string) (uint64, error) {
	v, set, err := envconfig.Uint(key)
	if err != nil {
		return 0, err
	}
	if !set {
		return 0, nil
	}
	if v == 0 || v > quicvarint.Max {
		return 0, fmt.Errorf("invalid %s %d: want 1 to %d; unset it for the default", key, v, uint64(quicvarint.Max))
	}
	return v, nil
}

// envSwitch reads key as a switch, with the words envBoolDefault takes: "1",
// "true", "yes" or "on", and "0", "false", "no" or "off", in any case. Unset
// or empty is def. Unlike envBoolDefault it refuses anything else.
func envSwitch(key string, def bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("invalid %s %q: want 1, true, yes or on, or 0, false, no or off", key, raw)
}
