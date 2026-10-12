package relay

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/stretchr/testify/assert"
)

func TestUpgradeClientOf(t *testing.T) {
	tests := map[string]struct {
		transport string
		header    http.Header
		want      upgradeClient
	}{
		"a WebTransport client that offers subprotocols": {
			transport: auth.TransportWebTransport,
			header: http.Header{
				"Wt-Available-Protocols": {`"moq-lite-05", "moq-lite-04"`},
				"User-Agent":             {"Mozilla/5.0 Chrome/153"},
				"Origin":                 {"https://app.example"},
			},
			want: upgradeClient{offered: `"moq-lite-05", "moq-lite-04"`, userAgent: "Mozilla/5.0 Chrome/153", origin: "https://app.example"},
		},
		"a WebTransport client that offers none": {
			transport: auth.TransportWebTransport,
			header:    http.Header{"Sec-Websocket-Protocol": {"qmux-02.moq-lite-05"}},
			want:      upgradeClient{},
		},
		"a WebSocket client": {
			transport: auth.TransportWebSocket,
			header:    http.Header{"Sec-Websocket-Protocol": {"qmux-02.moq-lite-05", "other"}},
			want:      upgradeClient{offered: "qmux-02.moq-lite-05, other"},
		},
		"control characters are dropped": {
			transport: auth.TransportWebTransport,
			header:    http.Header{"User-Agent": {"a\r\nlevel=ERROR\tb"}},
			want:      upgradeClient{userAgent: "alevel=ERRORb"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodConnect, "/room/1", nil)
			r.Header = tt.header

			got := upgradeClientOf(r, tt.transport)

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLoggedHeader(t *testing.T) {
	tests := map[string]struct {
		in   string
		want string
	}{
		"empty":             {in: "", want: ""},
		"at the limit":      {in: strings.Repeat("a", maxLoggedHeaderBytes), want: strings.Repeat("a", maxLoggedHeaderBytes)},
		"over the limit":    {in: strings.Repeat("a", maxLoggedHeaderBytes+1), want: strings.Repeat("a", maxLoggedHeaderBytes)},
		"cut between runes": {in: strings.Repeat("a", maxLoggedHeaderBytes-1) + "あ", want: strings.Repeat("a", maxLoggedHeaderBytes-1)},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, loggedHeader(tt.in))
		})
	}
}

func TestSessionStartAttrs(t *testing.T) {
	h3 := &tls.ConnectionState{NegotiatedProtocol: moqt.NextProtoH3, Version: tls.VersionTLS13, ServerName: "relay.example"}
	browser := decidedAdmission(&auth.Grant{}, auth.Request{ID: "abc", Transport: auth.TransportWebTransport, Path: "/room/1"})
	browser.client = upgradeClient{offered: `"moq-lite-05"`, userAgent: "Chrome/153", origin: "https://app.example"}
	silent := decidedAdmission(nil, auth.Request{ID: "def", Transport: auth.TransportWebTransport, Path: "/room/1"})
	internal := pendingAdmission()
	internal.decideInternal()

	tests := map[string]struct {
		admission *admission
		state     moqt.ConnectionState
		checked   bool
		want      []any
	}{
		"a browser over WebTransport": {
			admission: browser,
			state:     moqt.ConnectionState{Version: moqt.NextProtoMOQ, TLS: h3},
			checked:   true,
			want: []any{
				"id", "abc", "transport", auth.TransportWebTransport, "remote", "192.0.2.1:5000",
				"checked", true, "moq", moqt.NextProtoMOQ, "path", "/room/1",
				"alpn", moqt.NextProtoH3, "tls", "TLS 1.3", "sni", "relay.example",
				"offered", `"moq-lite-05"`, "user_agent", "Chrome/153", "origin", "https://app.example",
			},
		},
		"a client that offered no subprotocol says so": {
			admission: silent,
			state:     moqt.ConnectionState{Version: moqt.NextProtoMOQ},
			want: []any{
				"id", "def", "transport", auth.TransportWebTransport, "remote", "192.0.2.1:5000",
				"checked", false, "moq", moqt.NextProtoMOQ, "path", "/room/1", "offered", "",
			},
		},
		"a native-QUIC client has no offer": {
			admission: decidedAdmission(nil, auth.Request{ID: "ghi", Transport: auth.TransportQUIC, Path: "/room/1"}),
			state: moqt.ConnectionState{
				Version: moqt.NextProtoMOQ,
				TLS:     &tls.ConnectionState{NegotiatedProtocol: moqt.NextProtoMOQ, Version: tls.VersionTLS13},
			},
			want: []any{
				"id", "ghi", "transport", auth.TransportQUIC, "remote", "192.0.2.1:5000",
				"checked", false, "moq", moqt.NextProtoMOQ, "path", "/room/1",
				"alpn", moqt.NextProtoMOQ, "tls", "TLS 1.3",
			},
		},
		"an internal client has no id": {
			admission: internal,
			state:     moqt.ConnectionState{Version: moqt.NextProtoMOQ},
			want:      []any{"transport", transportInternal, "remote", "192.0.2.1:5000", "checked", false, "moq", moqt.NextProtoMOQ},
		},
		"a peer this relay dialed": {
			state: moqt.ConnectionState{Version: moqt.NextProtoMOQ},
			want:  []any{"transport", transportPeer, "remote", "192.0.2.1:5000", "checked", false, "moq", moqt.NextProtoMOQ},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := sessionStartAttrs(tt.admission, tt.state, "192.0.2.1:5000", tt.checked)

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSessionEndAttrs(t *testing.T) {
	a := decidedAdmission(&auth.Grant{}, auth.Request{ID: "abc", Transport: auth.TransportWebSocket})
	stats := moqt.SessionStats{BytesSent: 10, BytesReceived: 20}

	got := sessionEndAttrs(a, "192.0.2.1:5000", endDropped, 1500*time.Millisecond+300*time.Microsecond, stats)

	assert.Equal(t, []any{
		"id", "abc", "transport", auth.TransportWebSocket, "remote", "192.0.2.1:5000",
		"reason", endDropped, "duration", 1500 * time.Millisecond,
		"bytes_sent", stats.BytesSent, "bytes_received", stats.BytesReceived,
	}, got)
}
