// Package auth is the relay's session auth, and the "qumo auth" command.
//
// The relay verifies a session's credential itself (Verifier): it checks the
// token in the connect URL, signed with the token package, against a key set
// it loads from a file or a URL, and re-checks live sessions against it.
//
// "qumo auth" (Run) sets that up: keygen writes a signing key pair, and token
// signs a credential by hand.
package auth

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/qumo-dev/qumo/token"
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
	// TransportWebSocket is QMux over WebSocket, the path of clients
	// without a working WebTransport.
	TransportWebSocket = "websocket"
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
	// OptIns is how long the session has kept broadcasts opted into each
	// egress, added up over its broadcasts, set on revalidate and end
	// (usage.OptInSeconds). An egress it opted nothing into is absent.
	OptIns map[token.OptIn]time.Duration
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
