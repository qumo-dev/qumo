// Package usage is the format of the usage reports a qumo relay sends to
// QUMO_USAGE_URL: every 10 s, a POST of a JSON array of Records.
//
// A session that publishes a broadcast is reported by itself: SessionOpen
// when it is admitted, Usage with its cumulative bytes as it runs, and
// SessionClose with its final bytes and why it ended. Every other session is
// a viewer, and viewers are reported together: their bytes are added up per
// key into one running total for the relay's run, sent as Viewers. Viewers
// outnumber publishers by orders of magnitude, so a record per viewer would
// make a receiver's load grow with the audience.
//
// Byte counts are cumulative and from the relay's point of view (what it
// received is ingress), so a receiver can take a resent record without
// counting it twice. A relay that restarts begins a new Run, whose viewer
// totals start again from zero rather than lowering the old ones.
package usage

import (
	"errors"
	"fmt"
	"time"
)

// Type is what a record reports.
type Type string

// The record types.
const (
	// SessionOpen is a publishing session admitted.
	SessionOpen Type = "session_open"
	// Usage is a publishing session's cumulative bytes so far.
	Usage Type = "usage"
	// SessionClose is a publishing session's end, with its final bytes and
	// its Reason.
	SessionClose Type = "session_close"
	// Viewers is the cumulative bytes of every viewer session of one key in
	// one Run of the relay.
	Viewers Type = "viewers"
)

// The metrics a record carries, in bytes.
const (
	// IngressBytes is what the relay received.
	IngressBytes = "gateway.ingress_bytes"
	// EgressBytes is what the relay sent.
	EgressBytes = "gateway.egress_bytes"
)

// Why a publishing session ended, the Reason of its SessionClose.
const (
	// ReasonRefused is a session the relay ended because a re-check refused
	// its credential, as when its key left the key set.
	ReasonRefused = "refused"
	// ReasonClosed is a session the client or the relay closed normally.
	ReasonClosed = "closed"
	// ReasonDropped is a session whose connection was lost.
	ReasonDropped = "dropped"
	// ReasonUpgradeFailed is a WebTransport session that was admitted but
	// whose upgrade then failed, so it never started.
	ReasonUpgradeFailed = "upgrade_failed"
)

// Record is one record of a report.
type Record struct {
	Type Type `json:"type"`
	// SessionID names the publishing session of a SessionOpen, Usage or
	// SessionClose.
	SessionID string `json:"session_id,omitempty"`
	// Run names the run of the relay a Viewers total belongs to.
	Run string `json:"run,omitempty"`
	// KID is the key that signed the sessions' credentials.
	KID string `json:"kid"`
	// Metrics are cumulative byte counts, on every record but a SessionOpen.
	Metrics map[string]int64 `json:"metrics,omitempty"`
	// Reason is why a SessionClose's session ended.
	Reason string `json:"reason,omitempty"`
	// TS is when the relay wrote the record, to the second.
	TS time.Time `json:"ts"`
}

// Validate reports whether r has the fields its type needs: a KID and a TS;
// a SessionID on a session record and a Run on Viewers, never both; and
// Metrics, none negative, on every record but a SessionOpen.
func (r Record) Validate() error {
	switch r.Type {
	case SessionOpen, Usage, SessionClose:
		if r.SessionID == "" {
			return fmt.Errorf("usage: a %s record needs a session_id", r.Type)
		}
		if r.Run != "" {
			return fmt.Errorf("usage: a %s record has no run", r.Type)
		}
	case Viewers:
		if r.Run == "" {
			return errors.New("usage: a viewers record needs a run")
		}
		if r.SessionID != "" {
			return errors.New("usage: a viewers record has no session_id")
		}
	default:
		return fmt.Errorf("usage: unknown record type %q", r.Type)
	}
	if r.KID == "" {
		return fmt.Errorf("usage: a %s record needs a kid", r.Type)
	}
	if r.TS.IsZero() {
		return fmt.Errorf("usage: a %s record needs a ts", r.Type)
	}
	if r.Type != SessionOpen && len(r.Metrics) == 0 {
		return fmt.Errorf("usage: a %s record needs metrics", r.Type)
	}
	for name, v := range r.Metrics {
		if v < 0 {
			return fmt.Errorf("usage: metric %s is negative", name)
		}
	}
	return nil
}
