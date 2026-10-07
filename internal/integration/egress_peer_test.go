//go:build integration

package integration

import (
	"context"
	"crypto/tls"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/internal/devcert"
	"github.com/qumo-dev/qumo/internal/tlsclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEgress_ClientCertificateIsInternalClient verifies the HLS egress's
// connection (#432): with the client certificate it loads through
// tlsclient.ApplyClientCert (RELAY_CERT_FILE, RELAY_KEY_FILE) from the CA the
// relay trusts, it subscribes outside any grant without a credential, and the
// credential is never checked. Without the certificate it is refused. Its
// certificate carries no peering name, so it is an internal client, not a
// peer: it may announce nothing.
func TestEgress_ClientCertificateIsInternalClient(t *testing.T) {
	ca := newTestCA(t)
	egress := ca.issue(t, "egress-1", false)
	certFile, keyFile := writeCert(t, egress)

	// Authorize refuses everyone: only a session the CA vouches for gets
	// through.
	server := &fakeAuth{err: auth.RefusedError{Status: http.StatusUnauthorized}}
	publisher := &fakeAuth{grant: testGrant(t, "**", "", time.Time{}, 0)}
	addr, srv := startAuthRelay(t, func(ctx context.Context, req auth.Request) (*auth.Grant, error) {
		if req.Query == "jwt=publisher" {
			return publisher.authorize(ctx, req)
		}
		return server.authorize(ctx, req)
	}, ca.trust(t, nil))
	publishOver(t, srv, "https://"+addr+"/?jwt=publisher", "/hls/live")

	tests := map[string]struct {
		certFile, keyFile string
		wantTrusted       bool
	}{
		"with the CA-issued certificate": {certFile: certFile, keyFile: keyFile, wantTrusted: true},
		"without a certificate":          {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			asked := len(server.received())
			authorized := subscribeAuthorizations(t)
			tc := &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // test-only self-signed cert
				MinVersion:         tls.VersionTLS13,
			}
			require.NoError(t, tlsclient.ApplyClientCert(tc, tt.certFile, tt.keyFile))

			err := subscribeWith(t, nativeURL(addr)+"/", tc, "/hls/live")

			if tt.wantTrusted {
				assert.NoError(t, err, "an internal client subscribes outside any grant")
				assert.Len(t, server.received(), asked, "an internal client is never asked about")
				assert.Equal(t, authorized, subscribeAuthorizations(t), "its subscription is not counted as checked against a credential")
				return
			}
			assert.Error(t, err)
		})
	}

	t.Run("an internal client cannot announce", func(t *testing.T) {
		const path = moqt.BroadcastPath("/hls/from-egress")
		require.NoError(t, announceOver(t, nativeURL(addr)+"/", []string{moqt.NextProtoMOQ}, &egress, path))

		assert.Never(t, routed(srv, path), 1500*time.Millisecond, 25*time.Millisecond, "the announcement is refused")
	})
}

// A session the relay refuses is closed before its subscribes are answered:
// a subscribe sent while admission was pending fails with the relay's
// "unauthorized", not with "track does not exist" for a path that may well
// exist.
func TestRelay_RefusedSessionTellsItsSubscribeWhy(t *testing.T) {
	server := &fakeAuth{err: auth.RefusedError{Status: http.StatusUnauthorized}, hold: make(chan struct{})}
	publisher := &fakeAuth{grant: testGrant(t, "**", "", time.Time{}, 0)}
	addr, srv := startAuthRelay(t, func(ctx context.Context, req auth.Request) (*auth.Grant, error) {
		if req.Query == "jwt=publisher" {
			return publisher.authorize(ctx, req)
		}
		return server.authorize(ctx, req)
	}, nil)
	// The path exists: only the session's admission stands between the
	// subscribe and the broadcast.
	publishOver(t, srv, "https://"+addr+"/?jwt=publisher", "/hls/live")
	tc := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // test-only self-signed cert
		MinVersion:         tls.VersionTLS13,
	}
	subscribed := make(chan error, 1)
	go func() { subscribed <- subscribeWith(t, nativeURL(addr)+"/", tc, "/hls/live") }()
	require.Eventually(t, func() bool { return len(server.received()) == 1 }, 5*time.Second, 10*time.Millisecond,
		"the relay asks about the session")
	// The subscribe waits for the admission, which is what puts it in
	// flight when the refusal comes.
	require.Never(t, func() bool { return len(subscribed) > 0 }, 300*time.Millisecond, 10*time.Millisecond,
		"the subscribe is held while admission is pending")

	close(server.hold)

	var err error
	require.Eventually(t, func() bool {
		select {
		case err = <-subscribed:
			return true
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond, "the subscribe ends once the session is refused")
	var appErr *quic.ApplicationError
	require.ErrorAs(t, err, &appErr, "the subscribe fails with the relay's close")
	assert.True(t, appErr.Remote)
	assert.Equal(t, quic.ApplicationErrorCode(moqt.UnauthorizedSessionErrorCode), appErr.ErrorCode)
}

// subscribeAuthorizations returns how many subscriptions the relay has
// checked against a session's grant and admitted, from the metric it
// publishes.
func subscribeAuthorizations(tb testing.TB) float64 {
	tb.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(tb, err)
	for _, family := range families {
		if family.GetName() != "qumo_relay_subscribe_authorizations_total" {
			continue
		}
		for _, m := range family.GetMetric() {
			for _, label := range m.GetLabel() {
				if label.GetName() == "result" && label.GetValue() == "admitted" {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// writeCert writes a certificate and its key as PEM files for a client that
// loads them from disk.
func writeCert(t *testing.T, cert tls.Certificate) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key")
	certPEM, keyPEM, err := devcert.EncodePEM(cert)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(certFile, certPEM, 0o600))
	require.NoError(t, os.WriteFile(keyFile, keyPEM, 0o600))
	return certFile, keyFile
}

// subscribeWith subscribes to testTrack at path over a new session to url
// dialed with tc, and returns nil once a group arrives, or the error that
// refused it.
func subscribeWith(t *testing.T, url string, tc *tls.Config, path moqt.BroadcastPath) error {
	t.Helper()
	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := (&moqt.Dialer{TLSConfig: tc, QUICConfig: quicCfg}).Dial(ctx, url, moqt.NewTrackMux(0))
	if err != nil {
		return err
	}
	t.Cleanup(func() { _ = sess.CloseWithError(moqt.NoError, "test done") })
	tr, err := sess.Subscribe(ctx, path, testTrack, nil)
	if err != nil {
		return err
	}
	defer tr.Close()
	_, err = tr.AcceptGroup(ctx)
	return err
}
