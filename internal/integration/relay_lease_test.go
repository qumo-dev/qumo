//go:build integration

package integration

import (
	"context"
	"crypto/tls"
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

// leaseTransports are the two ways a client connects, each with a credential.
var leaseTransports = map[string]struct {
	url        func(addr string) string
	nextProtos []string
}{
	"WebTransport": {url: func(addr string) string { return "https://" + addr + "/acme?jwt=h.p.s" }},
	"native QUIC": {
		url:        func(addr string) string { return nativeURL(addr) + "/acme?jwt=h.p.s" },
		nextProtos: []string{moqt.NextProtoMOQ},
	},
}

// assertEndedBy asserts that the relay closed sess with Unauthorized and
// reason, which a client reads through moqt.Cause on either transport.
func assertEndedBy(t *testing.T, sess *moqt.Session, reason string) {
	t.Helper()
	var sessErr *moqt.SessionError
	require.ErrorAs(t, moqt.Cause(sess.Context()), &sessErr)
	assert.Equal(t, moqt.UnauthorizedSessionErrorCode, sessErr.SessionErrorCode())
	assert.Equal(t, reason, sessErr.ErrorMessage)
	assert.True(t, sessErr.Remote)
}

// TestRelay_RevalidateRefusedEndsSession verifies a live session is ended
// when it is refused at a revalidate, which is how key revocation reaches
// sessions already running, and that the client sees why: Unauthorized with
// the reason "refused", over WebTransport as over native QUIC.
func TestRelay_RevalidateRefusedEndsSession(t *testing.T) {
	for name, tt := range leaseTransports {
		t.Run(name, func(t *testing.T) {
			server := &fakeAuth{
				grant:         testGrant(t, "", "acme/**", time.Second),
				revalidateErr: auth.RefusedError{Status: http.StatusForbidden},
			}
			addr, _ := startAuthRelay(t, server.authorize, nil)

			sess := dialSession(t, tt.url(addr), tt.nextProtos)

			require.Eventually(t, func() bool { return sess.Context().Err() != nil },
				5*time.Second, 50*time.Millisecond, "a refused revalidate did not end the session")
			assertEndedBy(t, sess, "refused")
		})
	}
}

// TestRelay_SessionEndReport verifies the relay reports the end of a checked
// session to End: the connect request's id, why it ended, and its
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
				grant:         testGrant(t, "", "acme/**", time.Second),
				revalidateErr: tt.revalidateErr,
			}
			if tt.closeByClient {
				// No revalidate: the client closes first.
				server.grant = testGrant(t, "", "acme/**", 0)
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
