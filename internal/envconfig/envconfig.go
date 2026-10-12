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
		return 0, false, fmt.Errorf("invalid %s %q: want a whole number, zero or more", key, raw)
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
	if err != nil || v < 0 {
		return 0, false, fmt.Errorf("invalid %s %q: want a duration of zero or more, such as 10s", key, raw)
	}
	return v, true, nil
}
