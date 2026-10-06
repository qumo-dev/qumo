//go:build integration

package integration

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/qumo-dev/qumo/internal/relay"
	"github.com/stretchr/testify/require"
)

// testCA is a relay CA for a test: it issues peer certificates (with the
// peering name) and internal clients' certificates (without).
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(tb testing.TB) *testCA {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(tb, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test relay CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(tb, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(tb, err)
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a certificate for identity signed by the CA, usable as a
// client and a server, carrying the peering name when peer is set.
func (ca *testCA) issue(tb testing.TB, identity string, peer bool) tls.Certificate {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(tb, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	require.NoError(tb, err)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: identity},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	if peer {
		tmpl.DNSNames = []string{relay.PeeringName}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	require.NoError(tb, err)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	cert.Leaf, err = x509.ParseCertificate(der)
	require.NoError(tb, err)
	return cert
}

// trust returns the relay's trust for this CA: with a peer identity when
// peerCert is set, or the CA alone.
func (ca *testCA) trust(tb testing.TB, peerCert *tls.Certificate) *relay.PeerTrust {
	tb.Helper()
	t, err := relay.NewPeerTrust(ca.pem, peerCert)
	require.NoError(tb, err)
	return t
}
