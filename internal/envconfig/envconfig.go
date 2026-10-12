// Package envconfig resolves configuration values from the process environment.
//
// Every qumo command (relay, ingest, the HLS egress, seed-moq) reads its
// settings from environment variables with a default, and that "return the env
// value, or a default when it is unset or empty" read is the same in each — so it
// lives here once rather than copy-pasted per command.
package envconfig

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// String returns the value of the environment variable key, or def when key is
// unset or empty. os.Getenv cannot distinguish a missing variable from one set
// to the empty string, so an explicitly empty value is treated as unset and the
// default is returned.
func String(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Uint reads the environment variable key as a whole number of zero or more.
// Space around the value is ignored, as an env file may leave some. set is
// false when key is unset or blank, so the caller keeps its default. The
// error names key and quotes the value.
func Uint(key string) (v uint64, set bool, err error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, false, nil
	}
	v, err = strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("invalid %s %q: want a whole number, zero or more: %w", key, raw, err)
	}
	return v, true, nil
}

// Duration reads the environment variable key as a Go duration of zero or
// more, such as "10s". Space around the value is ignored, as an env file may
// leave some. set is false when key is unset or blank, so the caller keeps
// its default. The error names key and quotes the value.
func Duration(key string) (v time.Duration, set bool, err error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, false, nil
	}
	v, err = time.ParseDuration(raw)
	if err != nil {
		return 0, false, fmt.Errorf("invalid %s %q: want a duration of zero or more, such as 10s: %w", key, raw, err)
	}
	if v < 0 {
		return 0, false, fmt.Errorf("invalid %s %q: want a duration of zero or more, such as 10s", key, raw)
	}
	return v, true, nil
}

// Bool reads the environment variable key as a switch, with the words Switch
// takes. Anything else, or nothing, is def: a value it does not know leaves
// the default in place instead of stopping the command.
func Bool(key string, def bool) bool {
	if v, ok := parseSwitch(os.Getenv(key)); ok {
		return v
	}
	return def
}

// Switch reads the environment variable key as a switch: "1", "true", "yes"
// or "on" is true, and "0", "false", "no" or "off" is false, in any case and
// with space around it ignored. Unset or blank is def. Unlike Bool it refuses
// anything else, with an error that names key and quotes the value.
func Switch(key string, def bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, ok := parseSwitch(raw)
	if !ok {
		return false, fmt.Errorf("invalid %s %q: want 1, true, yes or on, or 0, false, no or off", key, raw)
	}
	return v, nil
}

// parseSwitch reads the words of a switch. ok is false for anything else.
func parseSwitch(raw string) (v, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}
