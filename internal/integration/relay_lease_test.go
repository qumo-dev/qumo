//go:build integration

package integration

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/internal/relay"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRelay_SessionEndsAtExpires verifies the relay closes a session at its
// grant's expires and not before. On native QUIC the client also sees the
// reason, "expired"; a WebTransport client session's context does not carry
// the close reason, only that it ended.
func TestRelay_SessionEndsAtExpires(t *testing.T) {
	tests := map[string]struct {
		url        func(addr string) string
		nextProtos []string
		seesReason bool
	}{
		"WebTransport": {url: func(addr string) string { return "https://" + addr + "/acme?jwt=h.p.s" }},
		"native QUIC": {
			url:        func(addr string) string { return nativeURL(addr) + "/acme?jwt=h.p.s" },
			nextProtos: []string{moqt.NextProtoMOQ},
			seesReason: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// expires is in whole seconds, so the session lives 2 to 3 s.
			expires := time.Now().Add(3 * time.Second).Unix()
			server := &fakeAuth{body: fmt.Sprintf(`{"subscribe":["acme/**"],"expires":%d}`, expires)}
			addr, _ := startAuthRelay(t, server.authorize, nil)

			sess := dialSession(t, tt.url(addr), tt.nextProtos)

			assert.Never(t, func() bool { return sess.Context().Err() != nil },
				1500*time.Millisecond, 50*time.Millisecond, "the session ended before its expires")
			require.Eventually(t, func() bool { return sess.Context().Err() != nil },
				5*time.Second, 50*time.Millisecond, "the session outlived its expires")
			assert.WithinDuration(t, time.Unix(expires, 0), time.Now(), time.Second)
			if tt.seesReason {
				assert.ErrorContains(t, context.Cause(sess.Context()), "expired")
			}
		})
	}
}

// TestRelay_RevalidateRefusedEndsSession verifies a live session is ended
// when the auth server refuses it at a revalidate, which is how key
// revocation and project suspension reach sessions already running.
func TestRelay_RevalidateRefusedEndsSession(t *testing.T) {
	server := &fakeAuth{
		body:          fmt.Sprintf(`{"subscribe":["acme/**"],"expires":%d,"revalidate":1}`, time.Now().Add(time.Hour).Unix()),
		revalidateErr: auth.RefusedError{Status: http.StatusForbidden},
	}
	addr, _ := startAuthRelay(t, server.authorize, nil)

	sess := dialSession(t, nativeURL(addr)+"/acme?jwt=h.p.s", []string{moqt.NextProtoMOQ})

	require.Eventually(t, func() bool { return sess.Context().Err() != nil },
		5*time.Second, 50*time.Millisecond, "a refused revalidate did not end the session")
	assert.ErrorContains(t, context.Cause(sess.Context()), "refused")
}

// TestRelay_SessionEndReport verifies the relay reports the end of a checked
// session to its auth server: the connect request's id, why it ended, and its
// final byte totals.
func TestRelay_SessionEndReport(t *testing.T) {
	tests := map[string]struct {
		revalidateErr error
		closeByClient bool
		wantReason    string
	}{
		"the client closes": {closeByClient: true, wantReason: "closed"},
		"revalidate refused": {
			revalidateErr: auth.RefusedError{Status: http.StatusForbidden},
			wantReason:    "refused",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			server := &fakeAuth{
				body:          fmt.Sprintf(`{"subscribe":["acme/**"],"expires":%d,"revalidate":1}`, time.Now().Add(time.Hour).Unix()),
				revalidateErr: tt.revalidateErr,
			}
			if tt.closeByClient {
				// No revalidate: the client closes first.
				server.body = fmt.Sprintf(`{"subscribe":["acme/**"],"expires":%d}`, time.Now().Add(time.Hour).Unix())
			}
			addr, _ := startAuthRelay(t, server.authorize, nil, func(s *relay.Server) { s.End = server.end })

			sess := dialSession(t, nativeURL(addr)+"/acme?jwt=h.p.s", []string{moqt.NextProtoMOQ})
			if tt.closeByClient {
				// Close once the relay has admitted the session: one closed
				// before its connect never started, so it has no end.
				require.Eventually(t, func() bool { return len(server.received()) > 0 },
					5*time.Second, 25*time.Millisecond, "no connect")
				require.NoError(t, sess.CloseWithError(moqt.NoError, "done"))
			}

			// The readiness probes' sessions may report too; ours is the one
			// that connected with the credential.
			var end auth.Request
			require.Eventually(t, func() bool {
				var ok bool
				end, ok = endFor(server, "jwt=h.p.s")
				return ok
			}, 5*time.Second, 25*time.Millisecond, "no end report")
			connect := connectFor(t, server.received(), end.ID)
			assert.Equal(t, auth.EventEnd, end.Event)
			assert.Equal(t, tt.wantReason, end.Reason)
			assert.Equal(t, connect.Path, end.Path, "end re-sends the connect request")
			assert.Positive(t, end.Bytes.Sent, "SETUP alone sends bytes")
			assert.Positive(t, end.Bytes.Received)
		})
	}
}

// endFor returns the end report of the session that connected with query.
func endFor(server *fakeAuth, query string) (auth.Request, bool) {
	for _, end := range server.ended() {
		if end.Query == query {
			return end, true
		}
	}
	return auth.Request{}, false
}

// connectFor returns the connect request with id among requests.
func connectFor(t *testing.T, requests []auth.Request, id string) auth.Request {
	t.Helper()
	for _, req := range requests {
		if req.ID == id && req.Event == auth.EventConnect {
			return req
		}
	}
	t.Fatalf("no connect request with id %q", id)
	return auth.Request{}
}

// dialSession dials url and returns the session, closed when the test ends.
func dialSession(t *testing.T, url string, nextProtos []string) *moqt.Session {
	t.Helper()
	dialerTLS := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // test-only self-signed cert
		MinVersion:         tls.VersionTLS13,
		NextProtos:         nextProtos,
	}
	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	sess, err := (&moqt.Dialer{TLSConfig: dialerTLS, QUICConfig: quicCfg}).Dial(ctx, url, moqt.NewTrackMux(0))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.CloseWithError(moqt.NoError, "test done") })
	return sess
}
