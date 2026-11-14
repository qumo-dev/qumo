// Package watermark holds the relay's capacity watermarks
// (MAX_CLIENT_SESSIONS, MAX_EGRESS_KBPS): how they are read and where they
// put the relay's thresholds. The relay enforces them (internal/relay) and
// `qumo doctor` explains them, from this one definition.
package watermark

import (
	"fmt"
	"math"
	"os"
	"strconv"
)

// The environment variables that set the watermarks.
const (
	EnvMaxClientSessions = "MAX_CLIENT_SESSIONS"
	EnvMaxEgressKbps     = "MAX_EGRESS_KBPS"
)

// FromEnv reads a watermark from key: a whole number, 0 (also when unset)
// for no limit. Anything else is an error, which stops the relay.
func FromEnv(key string) (int64, error) {
	return Parse(key, os.Getenv(key))
}

// Parse reads value as the watermark key: a whole number, empty or 0 for
// no limit.
func Parse(key, value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid %s %q: want a whole number, 0 for no limit", key, value)
	}
	return n, nil
}

// Ceiling is where a relay refuses new client sessions: max plus a tenth
// of it, rounded up, so that a relay is offered last (full) before it
// refuses. It saturates rather than overflow, so no limit, however large,
// refuses everything.
func Ceiling(max int64) int64 {
	extra := tenth(max)
	if max > math.MaxInt64-extra {
		return math.MaxInt64
	}
	return max + extra
}

// Room is the value at or below which a full relay has room again: 90% of
// max, rounded down, so that a value hovering at max does not make the
// relay full and not full on every reading.
func Room(max int64) int64 {
	return max - tenth(max)
}

// tenth is a tenth of max, rounded up, computed without overflow.
func tenth(max int64) int64 {
	t := max / 10
	if max%10 != 0 {
		t++
	}
	return t
}
