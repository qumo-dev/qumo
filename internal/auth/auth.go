// Package auth is the relay's session auth, and the "qumo auth" command.
//
// The relay verifies a session's credential itself (Verifier): it checks the
// token in the connect URL, signed with the token package, against a key set
// it loads from a file or a URL, re-checks live sessions against it, and can
// report each session's usage.
//
// "qumo auth" (Run) sets that up: keygen writes a signing key pair, and token
// signs a credential by hand.
package auth

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// maxBody bounds how much of a reply's body is drained.
const maxBody = 64 << 10

// Session events, the Request's Event.
const (
	// EventConnect asks whether a new session may start.
	EventConnect = "connect"
	// EventRevalidate asks again, at the grant's revalidate cadence, whether
	// a live session may continue.
	EventRevalidate = "revalidate"
	// EventEnd reports that a session ended, with its final byte totals.
	EventEnd = "end"
)

// Transports, the Request's Transport.
const (
	TransportWebTransport = "webtransport"
	TransportQUIC         = "quic"
)

// RefusedError is an explicit refusal of a session, with the HTTP status a
// WebTransport client gets: 401 for a credential that can't be accepted, 403
// for a valid one that may not do what it asks.
type RefusedError struct {
	Status int
}

func (e RefusedError) Error() string {
	return fmt.Sprintf("auth: refused (%d)", e.Status)
}

// RefusalStatus is the HTTP status a WebTransport client gets for err: the
// refusal's status, or 503 when the session could not be checked.
func RefusalStatus(err error) int {
	if refused, ok := errors.AsType[RefusedError](err); ok {
		return refused.Status
	}
	return http.StatusServiceUnavailable
}

// checkURL accepts an https URL, or http only on a loopback host: the bearer
// token the relay sends to it must not cross a network in the clear.
func checkURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "https":
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("http is allowed only for a loopback host, got %q", host)
		}
	default:
		return fmt.Errorf("want an https URL, got scheme %q", u.Scheme)
	}
	return nil
}

// noRedirectClient returns an HTTP client that never follows a redirect: a
// 307 or 308 would resend the request, and what it carries, to wherever
// Location points, past checkURL. A 3xx is answered like any other non-2xx.
func noRedirectClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Request is one session event: what the relay asks its Authorize about at
// connect and revalidate, and reports to its End.
type Request struct {
	ID        string
	Event     string
	Transport string
	Remote    string
	Path      string
	// Query is the connect URL's raw query, which carries the credential
	// (jwt). It is never logged.
	Query string

	// Bytes is the session's cumulative byte totals, set on revalidate and
	// end.
	Bytes Bytes
	// Reason is why the session ended, set on end.
	Reason string
}

// Bytes is a session's byte totals, both directions from the relay's point
// of view.
type Bytes struct {
	// Sent is the bytes the relay sent to the peer.
	Sent uint64
	// Received is the bytes the relay received from the peer.
	Received uint64
}
