// Package quicconfig reads the QUIC transport settings from the environment,
// so every qumo command that listens or dials over QUIC takes the same
// QUIC_* variables with the same checks.
package quicconfig

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/quicvarint"

	"github.com/qumo-dev/qumo/internal/envconfig"
)

const (
	// minInitialPacketSize is the smallest packet QUIC allows a connection
	// to start with (RFC 9000 §14.1), and maxInitialPacketSize the largest
	// quic-go sends. quic-go clamps a value outside them without saying so;
	// FromEnv refuses it instead.
	minInitialPacketSize = 1200
	maxInitialPacketSize = 1452

	// minDuration is the shortest keep-alive period or timeout taken. A peer
	// is told the idle timeout in whole milliseconds, where zero means none,
	// so a shorter one would be announced as no timeout.
	minDuration = time.Millisecond

	// quic-go's idle timeout and receive windows when a setting is left
	// unset: what a keep-alive period and an unset half of an initial/max
	// pair are checked against. They are copied from quic-go's
	// internal/protocol, like the packet size range above, and
	// TestFromEnv_QUICGoVersion fails when go.mod moves to another release,
	// so they are read again.
	quicGoIdleTimeout             = 30 * time.Second
	quicGoInitialStreamWindow     = 512 << 10
	quicGoMaxStreamWindow         = 6 << 20
	quicGoInitialConnectionWindow = 768 << 10
	quicGoMaxConnectionWindow     = 15 << 20
)

// EnvSet returns the QUIC_* settings present in the environment as
// "NAME=value", for a startup log: a setting that never reached the process
// fails the same way as one left unset. A blank value counts as unset, as it
// does when the setting is read, and line breaks are taken out of a value.
func EnvSet() []string {
	var set []string
	for _, key := range envconfig.QUICKeys() {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			set = append(set, key+"="+strings.NewReplacer("\r", "", "\n", "").Replace(v))
		}
	}
	return set
}

// LogEnvSet logs the QUIC_* settings found in the environment, if any, as
// command's startup line.
func LogEnvSet(command string) {
	if set := EnvSet(); len(set) > 0 {
		slog.Info(command+": QUIC transport settings from the environment", "set", set)
	}
}

// FromEnv returns a copy of base with the QUIC_* settings found in the
// environment applied. base carries what the command fixes or defaults by
// itself and may be nil. Unset, a setting keeps base's value or, where base
// has none, quic-go's.
//
//	QUIC_INITIAL_PACKET_SIZE           - UDP payload bytes a connection starts with, 1200 to 1452 (quic-go: 1280)
//	QUIC_PMTUD                         - a switch; off keeps packets at the initial size
//	QUIC_KEEPALIVE                     - keep-alive interval, e.g. "10s", at most half the idle timeout; "0" sends none
//	QUIC_IDLE_TIMEOUT                  - close a session silent for this long, 1ms or more (quic-go: 30s)
//	QUIC_HANDSHAKE_IDLE_TIMEOUT        - give up a handshake silent for this long, 1ms or more (quic-go: 5s)
//	QUIC_STREAM_RECEIVE_WINDOW         - bytes (quic-go: 512 KB)
//	QUIC_MAX_STREAM_RECEIVE_WINDOW     - bytes, no less than the initial one (quic-go: 6 MB)
//	QUIC_CONNECTION_RECEIVE_WINDOW     - bytes (quic-go: 768 KB)
//	QUIC_MAX_CONNECTION_RECEIVE_WINDOW - bytes, no less than the initial one (quic-go: 15 MB)
//
// Refused, because quic-go would take each as something else without saying
// so: an initial packet size outside its range, which it clamps; a timeout of
// zero, which it reads as its own default; a window above the largest QUIC
// varint, which it panics on at the first handshake; a max window below the
// initial one in effect, which it never shrinks to; and a keep-alive period
// above half the idle timeout, which it shortens to that half. What quic-go
// adjusts while a connection runs is not checked: it never times out or pings
// faster than a few probe timeouts allow, and it shortens the idle timeout
// and the keep-alive to what the peer announces.
//
// A keep-alive period base sets is kept within half of an idle timeout the
// environment shortens, as quic-go would do.
func FromEnv(base *quic.Config) (*quic.Config, error) {
	cfg := &quic.Config{}
	if base != nil {
		cfg = base.Clone()
	}

	size, set, err := envconfig.Uint(envconfig.QUICInitialPacketSize)
	if err != nil {
		return nil, err
	}
	if set {
		if size < minInitialPacketSize || size > maxInitialPacketSize {
			return nil, fmt.Errorf("invalid %s %d: want %d to %d",
				envconfig.QUICInitialPacketSize, size, minInitialPacketSize, maxInitialPacketSize)
		}
		cfg.InitialPacketSize = uint16(size)
	}

	pmtud, err := envconfig.Switch(envconfig.QUICPMTUD, !cfg.DisablePathMTUDiscovery)
	if err != nil {
		return nil, err
	}
	cfg.DisablePathMTUDiscovery = !pmtud

	if err := setTimeout(envconfig.QUICIdleTimeout, &cfg.MaxIdleTimeout); err != nil {
		return nil, err
	}
	if err := setTimeout(envconfig.QUICHandshakeIdleTimeout, &cfg.HandshakeIdleTimeout); err != nil {
		return nil, err
	}
	idleTimeout := cfg.MaxIdleTimeout
	if idleTimeout == 0 {
		idleTimeout = quicGoIdleTimeout
	}
	if cfg.KeepAlivePeriod, err = keepAlive(cfg.KeepAlivePeriod, idleTimeout); err != nil {
		return nil, err
	}

	if err := setWindows(
		envconfig.QUICStreamReceiveWindow, envconfig.QUICMaxStreamReceiveWindow,
		&cfg.InitialStreamReceiveWindow, &cfg.MaxStreamReceiveWindow,
		quicGoInitialStreamWindow, quicGoMaxStreamWindow,
	); err != nil {
		return nil, err
	}
	if err := setWindows(
		envconfig.QUICConnectionReceiveWindow, envconfig.QUICMaxConnectionReceiveWindow,
		&cfg.InitialConnectionReceiveWindow, &cfg.MaxConnectionReceiveWindow,
		quicGoInitialConnectionWindow, quicGoMaxConnectionWindow,
	); err != nil {
		return nil, err
	}
	return cfg, nil
}

// setTimeout reads key into dst, leaving dst alone when key is unset. It
// refuses zero, which quic-go reads as "use my default", not "no timeout",
// and anything under minDuration.
func setTimeout(key string, dst *time.Duration) error {
	v, set, err := envconfig.Duration(key)
	if err != nil {
		return err
	}
	if !set {
		return nil
	}
	if v < minDuration {
		return fmt.Errorf("invalid %s %s: want %s or more; unset it for the default", key, v, minDuration)
	}
	*dst = v
	return nil
}

// keepAlive reads the keep-alive period. Unset, it is def, or half of
// idleTimeout when that is less. Zero sends no keep-alives. Otherwise it is
// minDuration or more and at most half of idleTimeout: quic-go pings no less
// often than that, whatever is asked.
func keepAlive(def, idleTimeout time.Duration) (time.Duration, error) {
	v, set, err := envconfig.Duration(envconfig.QUICKeepAlive)
	if err != nil {
		return 0, err
	}
	if !set {
		return min(def, idleTimeout/2), nil
	}
	if v == 0 {
		return 0, nil
	}
	if v < minDuration {
		return 0, fmt.Errorf("invalid %s %s: want 0 for none, or %s or more", envconfig.QUICKeepAlive, v, minDuration)
	}
	if v > idleTimeout/2 {
		return 0, fmt.Errorf("invalid %s %s: more than half the idle timeout %s (%s); a session would be pinged every %s whatever is set here",
			envconfig.QUICKeepAlive, v, idleTimeout, envconfig.QUICIdleTimeout, idleTimeout/2)
	}
	return v, nil
}

// setWindows reads an initial/max pair of receive windows into initial and
// maxWindow, leaving each alone when its key is unset. A window still zero
// is left to quic-go, whose defaults are defInitial and defMax. It refuses a
// window larger than a QUIC varint holds, which quic-go would panic on at the
// first handshake, and an initial window above the max one in effect, which
// quic-go would then ignore. The error names a variable that is set.
func setWindows(initialKey, maxKey string, initial, maxWindow *uint64, defInitial, defMax uint64) error {
	envInitial, err := window(initialKey)
	if err != nil {
		return err
	}
	envMax, err := window(maxKey)
	if err != nil {
		return err
	}
	effInitial, effMax := *initial, *maxWindow
	if envInitial != 0 {
		effInitial = envInitial
	}
	if envMax != 0 {
		effMax = envMax
	}
	if effInitial == 0 {
		effInitial = defInitial
	}
	if effMax == 0 {
		effMax = defMax
	}
	if (envInitial != 0 || envMax != 0) && effInitial > effMax {
		// Blame a variable the operator set: the max one when it is set,
		// the initial one otherwise.
		if envMax != 0 {
			return fmt.Errorf("invalid %s %d: less than the initial window %d in effect (%s); the window starts at the initial size and never shrinks",
				maxKey, envMax, effInitial, initialKey)
		}
		return fmt.Errorf("invalid %s %d: more than the max window %d in effect; set %s to at least as much",
			initialKey, envInitial, effMax, maxKey)
	}
	if envInitial != 0 {
		*initial = envInitial
	}
	if envMax != 0 {
		*maxWindow = envMax
	}
	return nil
}

// window reads one receive window: zero when unset, otherwise 1 to the
// largest QUIC varint.
func window(key string) (uint64, error) {
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
