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
