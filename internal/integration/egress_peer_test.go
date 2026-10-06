//go:build integration

package integration

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
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

	// Authorize refuses everyone: only a trusted peer gets through.
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
			tc := &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // test-only self-signed cert
				MinVersion:         tls.VersionTLS13,
			}
			require.NoError(t, tlsclient.ApplyClientCert(tc, tt.certFile, tt.keyFile))

			err := subscribeWith(t, nativeURL(addr)+"/", tc, "/hls/live")

			if tt.wantTrusted {
				assert.NoError(t, err, "an internal client subscribes outside any grant")
				assert.Len(t, server.received(), asked, "an internal client is never asked about")
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

// writeCert writes a certificate and its key as PEM files for a client that
// loads them from disk.
func writeCert(t *testing.T, cert tls.Certificate) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key")
	keyDER, err := x509.MarshalECPrivateKey(cert.PrivateKey.(*ecdsa.PrivateKey))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
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
