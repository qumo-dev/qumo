package hls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// relayTLSConfig is the egress's verification policy turned into a tls.Config:
// verify against the system roots by default, trust a named relay cert when one
// is given, or skip verification entirely when insecure is set. insecure
// dominates a caFile, matching crypto/tls.
func TestRelayTLSConfig(t *testing.T) {
	cert := writeCertFile(t, "relay.pem")
	bogus := filepath.Join(t.TempDir(), "absent.pem") // never written, so missing

	t.Run("system roots by default", func(t *testing.T) {
		tc, err := relayTLSConfig("", false)
		require.NoError(t, err)
		assert.Equal(t, uint16(tls.VersionTLS13), tc.MinVersion)
		assert.False(t, tc.InsecureSkipVerify, "verification is on by default")
		assert.Nil(t, tc.RootCAs, "nil RootCAs means crypto/tls uses the system root store")
	})

	t.Run("trusts a named relay cert", func(t *testing.T) {
		tc, err := relayTLSConfig(cert, false)
		require.NoError(t, err)
		assert.False(t, tc.InsecureSkipVerify)
		require.NotNil(t, tc.RootCAs, "a named cert overrides the system roots with its own pool")
	})

	t.Run("insecure skips verification", func(t *testing.T) {
		tc, err := relayTLSConfig("", true)
		require.NoError(t, err)
		assert.True(t, tc.InsecureSkipVerify)
		assert.Nil(t, tc.RootCAs)
	})

	t.Run("insecure dominates a bad caFile", func(t *testing.T) {
		// The bogus path is never read: insecure short-circuits before loading.
		tc, err := relayTLSConfig(bogus, true)
		require.NoError(t, err)
		assert.True(t, tc.InsecureSkipVerify)
	})

	t.Run("missing caFile is an error", func(t *testing.T) {
		_, err := relayTLSConfig(bogus, false)
		assert.Error(t, err)
	})

	t.Run("caFile without a certificate is an error", func(t *testing.T) {
		empty := filepath.Join(t.TempDir(), "empty.pem")
		require.NoError(t, os.WriteFile(empty, []byte("not a certificate"), 0o600))
		_, err := relayTLSConfig(empty, false)
		assert.Error(t, err)
	})
}

// writeCertFile writes a self-signed PEM cert to a temp file and returns its
// path. Mirrors the relay cert a dev relay (or `mage cert`) would present.
func writeCertFile(tb testing.TB, name string) string {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), name)
	require.NoError(tb, os.WriteFile(path, selfSignedCertPEM(tb), 0o600))
	return path
}

// selfSignedCertPEM generates a throwaway self-signed certificate as PEM — the
// same shape internal/loadgen uses for its --ca fixture.
func selfSignedCertPEM(tb testing.TB) []byte {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(tb, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(tb, err)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(tb, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestFeedConfig_Validate(t *testing.T) {
	tests := map[string]struct {
		cfg     feedConfig
		wantErr string
	}{
		"no client certificate":               {cfg: feedConfig{relayURL: "https://relay:4433"}},
		"a credential in the query":           {cfg: feedConfig{relayURL: "https://relay:4433/live?jwt=h.p.s"}},
		"a malformed URL":                     {cfg: feedConfig{relayURL: "moqt://[bad/live"}, wantErr: "invalid URL"},
		"client certificate over native QUIC": {cfg: feedConfig{relayURL: "moqt://relay:4433", certFile: "c.pem", keyFile: "k.pem"}},
		"client certificate over WebTransport": {
			cfg:     feedConfig{relayURL: "https://relay:4433", certFile: "c.pem", keyFile: "k.pem"},
			wantErr: "moqt://",
		},
		"cert without key": {cfg: feedConfig{relayURL: "moqt://relay:4433", certFile: "c.pem"}, wantErr: "together"},
		"key without cert": {cfg: feedConfig{relayURL: "moqt://relay:4433", keyFile: "k.pem"}, wantErr: "together"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := tt.cfg.validate()

			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			assert.NoError(t, err)
		})
	}
}

// TestFeedConfig_Validate_MalformedURLHidesCredential verifies a RELAY_URL
// that doesn't parse is reported without quoting it: url.Parse's error would
// quote its whole input, credential included.
func TestFeedConfig_Validate_MalformedURLHidesCredential(t *testing.T) {
	cfg := feedConfig{relayURL: "moqt://[bad/live?jwt=h.p.s"}

	err := cfg.validate()

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "jwt=h.p.s")
}
