package relay

import (
	"crypto/tls"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
)

// Headers an upgrade names the subprotocols it offers in.
const (
	headerWebTransportProtocols = "WT-Available-Protocols"
	headerWebSocketProtocols    = "Sec-WebSocket-Protocol"
)

// maxLoggedHeaderBytes bounds a request header's value in a log line.
const maxLoggedHeaderBytes = 256

// upgradeClient is what an upgrade request said about its client, kept for
// the session's log lines. A native-QUIC session has none.
type upgradeClient struct {
	// offered is the subprotocols the client offered, as it sent them.
	// Empty is a client that offered none, which the relay serves all the
	// same: nothing was agreed on what the session speaks.
	offered   string
	userAgent string
	origin    string
}

// upgradeClientOf reads what the upgrade r, to transport, said about its
// client.
func upgradeClientOf(r *http.Request, transport string) upgradeClient {
	header := headerWebTransportProtocols
	if transport == auth.TransportWebSocket {
		header = headerWebSocketProtocols
	}
	return upgradeClient{
		offered:   loggedHeader(strings.Join(r.Header.Values(header), ", ")),
		userAgent: loggedHeader(r.UserAgent()),
		origin:    loggedHeader(r.Header.Get("Origin")),
	}
}

// loggedHeader makes a header value a client chose fit for a log line: no
// control characters, and at most maxLoggedHeaderBytes.
func loggedHeader(v string) string {
	v = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, v)
	if len(v) <= maxLoggedHeaderBytes {
		return v
	}
	cut := maxLoggedHeaderBytes
	for cut > 0 && !isRuneStart(v[cut]) {
		cut--
	}
	return v[:cut]
}

// isRuneStart reports whether b begins a UTF-8 sequence.
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// sessionStartAttrs describes a session the relay starts to serve: whom the
// relay takes it for, how it reached the relay, and what the two ends speak. moq is the MoQ
// version the relay speaks on it; alpn and offered are what the client
// asked for.
func sessionStartAttrs(a *admission, state moqt.ConnectionState, remote string, checked bool) []any {
	attrs := append(sessionIdentityAttrs(a, remote), "checked", checked, "moq", state.Version)
	if a != nil {
		if a.req.Path != "" {
			attrs = append(attrs, "path", a.req.Path)
		}
		if a.grant != nil && a.grant.Subject() != "" {
			attrs = append(attrs, "sub", a.grant.Subject())
		}
	}
	if t := state.TLS; t != nil {
		attrs = append(attrs, "alpn", t.NegotiatedProtocol, "tls", tls.VersionName(t.Version))
		if t.ServerName != "" {
			attrs = append(attrs, "sni", t.ServerName)
		}
	}
	if a != nil && isUpgrade(a.req.Transport) {
		attrs = append(attrs, "offered", a.client.offered)
		if a.client.userAgent != "" {
			attrs = append(attrs, "user_agent", a.client.userAgent)
		}
		if a.client.origin != "" {
			attrs = append(attrs, "origin", a.client.origin)
		}
	}
	return attrs
}

// sessionEndAttrs describes a session that ended: why, how long it ran and
// what it carried.
func sessionEndAttrs(a *admission, remote, reason string, ran time.Duration, stats moqt.SessionStats) []any {
	return append(sessionIdentityAttrs(a, remote),
		"reason", reason,
		"duration", ran.Round(time.Millisecond),
		"bytes_sent", stats.BytesSent,
		"bytes_received", stats.BytesReceived,
	)
}

// sessionIdentityAttrs names a session the same way in every line about it.
// A session without a connect request (a relay peer, an internal client) has
// no id.
func sessionIdentityAttrs(a *admission, remote string) []any {
	attrs := make([]any, 0, 24)
	if a != nil && a.req.ID != "" {
		attrs = append(attrs, "id", a.req.ID)
	}
	return append(attrs, "class", admittedClass(a).label(), "transport", sessionTransport(a), "remote", remote)
}

// isUpgrade reports whether transport is one a session reaches through an
// HTTP upgrade.
func isUpgrade(transport string) bool {
	return transport == auth.TransportWebTransport || transport == auth.TransportWebSocket
}
