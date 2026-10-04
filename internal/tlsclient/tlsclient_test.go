package tlsclient

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

// Apply is the trust decision shared across qumo's relay clients, turned into a
// tls.Config: verify against the system roots by default, trust a named cert when
// one is given, or skip verification entirely when insecure is set. insecure
// dominates a caFile, matching crypto/tls.
func TestApply(t *testing.T) {
	cert := writeCertFile(t, "relay.pem")
	bogus := filepath.Join(t.TempDir(), "absent.pem") // never written, so missing

	t.Run("system roots by default", func(t *testing.T) {
		tc := &tls.Config{}
		require.NoError(t, Apply(tc, "", false))
		assert.False(t, tc.InsecureSkipVerify, "verification is on by default")
		assert.Nil(t, tc.RootCAs, "nil RootCAs means crypto/tls uses the system root store")
	})

	t.Run("trusts a named relay cert", func(t *testing.T) {
		tc := &tls.Config{}
		require.NoError(t, Apply(tc, cert, false))
		assert.False(t, tc.InsecureSkipVerify)
		require.NotNil(t, tc.RootCAs, "a named cert overrides the system roots with its own pool")
	})

	t.Run("insecure skips verification", func(t *testing.T) {
		tc := &tls.Config{}
		require.NoError(t, Apply(tc, "", true))
		assert.True(t, tc.InsecureSkipVerify)
		assert.Nil(t, tc.RootCAs)
	})

	t.Run("insecure dominates a bad caFile", func(t *testing.T) {
		// The bogus path is never read: insecure short-circuits before loading.
		tc := &tls.Config{}
		require.NoError(t, Apply(tc, bogus, true))
		assert.True(t, tc.InsecureSkipVerify)
	})

	t.Run("does not touch the caller's other fields", func(t *testing.T) {
		tc := &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{"moq"}}
		require.NoError(t, Apply(tc, cert, false))
		assert.Equal(t, uint16(tls.VersionTLS13), tc.MinVersion)
		assert.Equal(t, []string{"moq"}, tc.NextProtos)
	})

	t.Run("missing caFile is an error", func(t *testing.T) {
		tc := &tls.Config{}
		assert.Error(t, Apply(tc, bogus, false))
	})

	t.Run("caFile without a certificate is an error", func(t *testing.T) {
		empty := filepath.Join(t.TempDir(), "empty.pem")
		require.NoError(t, os.WriteFile(empty, []byte("not a certificate"), 0o600))
		tc := &tls.Config{}
		assert.Error(t, Apply(tc, empty, false))
	})
}

func TestLoadCAPool(t *testing.T) {
	dir := t.TempDir()
	certPEM := selfSignedCertPEM(t)
	good := filepath.Join(dir, "cert.pem")
	require.NoError(t, os.WriteFile(good, certPEM, 0o600))
	bad := filepath.Join(dir, "bad.pem")
	require.NoError(t, os.WriteFile(bad, []byte("not a pem"), 0o600))

	t.Run("valid cert", func(t *testing.T) {
		pool, err := loadCAPool(good)
		require.NoError(t, err)
		assert.NotNil(t, pool)
	})
	t.Run("empty path returns a nil pool, no error", func(t *testing.T) {
		// Empty means "verify against the system roots" — not a missing file.
		pool, err := loadCAPool("")
		require.NoError(t, err)
		assert.Nil(t, pool)
	})
	t.Run("no certs in file", func(t *testing.T) {
		_, err := loadCAPool(bad)
		assert.Error(t, err)
	})
	t.Run("missing file", func(t *testing.T) {
		_, err := loadCAPool(filepath.Join(dir, "absent.pem"))
		assert.Error(t, err)
	})
}

func writeCertFile(tb testing.TB, name string) string {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), name)
	require.NoError(tb, os.WriteFile(path, selfSignedCertPEM(tb), 0o600))
	return path
}

// selfSignedCertPEM generates a throwaway self-signed certificate as PEM — the
// same shape a dev relay (or `mage cert`) would present.
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

func TestApplyClientCert(t *testing.T) {
	certFile, keyFile := writeKeyPair(t)
	tests := map[string]struct {
		certFile, keyFile string
		wantCerts         int
		wantErr           bool
	}{
		"none":             {},
		"cert and key":     {certFile: certFile, keyFile: keyFile, wantCerts: 1},
		"cert without key": {certFile: certFile, wantErr: true},
		"key without cert": {keyFile: keyFile, wantErr: true},
		"missing files":    {certFile: certFile + ".absent", keyFile: keyFile, wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			tc := &tls.Config{}

			err := ApplyClientCert(tc, tt.certFile, tt.keyFile)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Len(t, tc.Certificates, tt.wantCerts)
		})
	}
}

// writeKeyPair writes a throwaway self-signed certificate and its key as PEM
// files and returns their paths.
func writeKeyPair(tb testing.TB) (certFile, keyFile string) {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(tb, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "egress"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(tb, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(tb, err)
	dir := tb.TempDir()
	certFile, keyFile = filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key")
	require.NoError(tb, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(tb, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	return certFile, keyFile
}
