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
	"github.com/qumo-dev/qumo/internal/tlsclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEgress_ClientCertificateIsTrustedPeer verifies the HLS egress's
// connection (#432): with the client certificate it loads through
// tlsclient.ApplyClientCert (RELAY_CERT_FILE, RELAY_KEY_FILE) from the CA the
// relay trusts, it subscribes outside any grant without a credential, and the
// credential is never checked. Without the certificate it is refused.
func TestEgress_ClientCertificateIsTrustedPeer(t *testing.T) {
	certFile, keyFile := createTempCert(t)
	peerCert, err := tls.LoadX509KeyPair(certFile, keyFile)
	require.NoError(t, err)

	// Authorize refuses everyone: only a trusted peer gets through.
	server := &fakeAuth{err: auth.RefusedError{Status: http.StatusUnauthorized}}
	publisher := &fakeAuth{grant: testGrant(t, "**", "", time.Time{}, 0)}
	addr, srv := startAuthRelay(t, func(ctx context.Context, req auth.Request) (*auth.Grant, error) {
		if req.Query == "jwt=publisher" {
			return publisher.authorize(ctx, req)
		}
		return server.authorize(ctx, req)
	}, &peerCert)
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
			tc := &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // test-only self-signed cert
				MinVersion:         tls.VersionTLS13,
			}
			require.NoError(t, tlsclient.ApplyClientCert(tc, tt.certFile, tt.keyFile))

			err := subscribeWith(t, nativeURL(addr)+"/", tc, "/hls/live")

			if tt.wantTrusted {
				assert.NoError(t, err, "a trusted peer subscribes outside any grant")
				assert.Len(t, server.received(), asked, "a trusted peer is never asked about")
				return
			}
			assert.Error(t, err)
		})
	}
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
