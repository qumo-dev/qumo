package relay

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/quic-go/quic-go"
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
)

// quicConfigFromEnv builds the QUIC configuration of the relay's listener and
// of its peer dialer from the environment. Unset, a setting keeps the relay's
// default or, where the relay has none, quic-go's.
//
//	RELAY_QUIC_INITIAL_PACKET_SIZE               - UDP payload bytes a connection starts with, 1200 to 1452 (quic-go: 1280)
//	RELAY_QUIC_DISABLE_PMTUD                     - "true" keeps packets at the initial size (default: false)
//	RELAY_QUIC_KEEPALIVE_PERIOD                  - keep-alive interval, e.g. "10s"; "0" sends none (default: 10s)
//	RELAY_QUIC_MAX_IDLE_TIMEOUT                  - close a session silent for this long (default: 60s)
//	RELAY_QUIC_HANDSHAKE_IDLE_TIMEOUT            - give up a handshake silent for this long (quic-go: 5s)
//	RELAY_QUIC_INITIAL_STREAM_RECEIVE_WINDOW     - bytes (quic-go: 512 KB)
//	RELAY_QUIC_MAX_STREAM_RECEIVE_WINDOW         - bytes (quic-go: 6 MB)
//	RELAY_QUIC_INITIAL_CONNECTION_RECEIVE_WINDOW - bytes (quic-go: 768 KB)
//	RELAY_QUIC_MAX_CONNECTION_RECEIVE_WINDOW     - bytes (quic-go: 15 MB)
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

	size, set, err := envUint("RELAY_QUIC_INITIAL_PACKET_SIZE")
	if err != nil {
		return nil, err
	}
	if set {
		if size < minQUICInitialPacketSize || size > maxQUICInitialPacketSize {
			return nil, fmt.Errorf("invalid RELAY_QUIC_INITIAL_PACKET_SIZE %d: want %d to %d",
				size, minQUICInitialPacketSize, maxQUICInitialPacketSize)
		}
		cfg.InitialPacketSize = uint16(size)
	}

	if raw := os.Getenv("RELAY_QUIC_DISABLE_PMTUD"); raw != "" {
		off, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid RELAY_QUIC_DISABLE_PMTUD %q: want true or false", raw)
		}
		cfg.DisablePathMTUDiscovery = off
	}

	durations := []struct {
		key string
		dst *time.Duration
	}{
		{"RELAY_QUIC_KEEPALIVE_PERIOD", &cfg.KeepAlivePeriod},
		{"RELAY_QUIC_MAX_IDLE_TIMEOUT", &cfg.MaxIdleTimeout},
		{"RELAY_QUIC_HANDSHAKE_IDLE_TIMEOUT", &cfg.HandshakeIdleTimeout},
	}
	for _, d := range durations {
		v, set, err := envDuration(d.key)
		if err != nil {
			return nil, err
		}
		if set {
			*d.dst = v
		}
	}

	windows := []struct {
		key string
		dst *uint64
	}{
		{"RELAY_QUIC_INITIAL_STREAM_RECEIVE_WINDOW", &cfg.InitialStreamReceiveWindow},
		{"RELAY_QUIC_MAX_STREAM_RECEIVE_WINDOW", &cfg.MaxStreamReceiveWindow},
		{"RELAY_QUIC_INITIAL_CONNECTION_RECEIVE_WINDOW", &cfg.InitialConnectionReceiveWindow},
		{"RELAY_QUIC_MAX_CONNECTION_RECEIVE_WINDOW", &cfg.MaxConnectionReceiveWindow},
	}
	for _, w := range windows {
		v, set, err := envUint(w.key)
		if err != nil {
			return nil, err
		}
		if set {
			*w.dst = v
		}
	}
	return cfg, nil
}

// envUint reads key as a whole number of zero or more. set is false when key
// is unset or empty.
func envUint(key string) (v uint64, set bool, err error) {
	raw := os.Getenv(key)
	if raw == "" {
		return 0, false, nil
	}
	v, err = strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("invalid %s %q: want a whole number, zero or more", key, raw)
	}
	return v, true, nil
}

// envDuration reads key as a Go duration of zero or more, such as "10s". set
// is false when key is unset or empty.
func envDuration(key string) (v time.Duration, set bool, err error) {
	raw := os.Getenv(key)
	if raw == "" {
		return 0, false, nil
	}
	v, err = time.ParseDuration(raw)
	if err != nil || v < 0 {
		return 0, false, fmt.Errorf("invalid %s %q: want a duration of zero or more, such as 10s", key, raw)
	}
	return v, true, nil
}
