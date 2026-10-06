package devcert

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	"github.com/qumo-dev/qumo/internal/relay"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The magefile can't import the relay, so the name is repeated in this
// package; this keeps the two from drifting.
func TestPeeringName(t *testing.T) {
	assert.Equal(t, relay.PeeringName, PeeringName)
}

func TestCA_Issue(t *testing.T) {
	ca, err := NewCA("test relay CA", time.Hour)
	require.NoError(t, err)
	intermediate, err := ca.NewIntermediate("test issuing CA", time.Hour)
	require.NoError(t, err)

	tests := map[string]struct {
		issuer      *CA
		leaf        Leaf
		wantChain   int
		wantErrText string // from the relay's check of it as a peer certificate; empty: accepted
	}{
		"a peer certificate":             {issuer: ca, leaf: Leaf{CommonName: "relay-1", DNSNames: []string{PeeringName}, Validity: time.Hour}, wantChain: 1},
		"through an intermediate":        {issuer: intermediate, leaf: Leaf{CommonName: "relay-1", DNSNames: []string{PeeringName}, Validity: time.Hour}, wantChain: 2},
		"an internal client's":           {issuer: ca, leaf: Leaf{CommonName: "egress-1", Validity: time.Hour}, wantChain: 1, wantErrText: "peering name"},
		"expired":                        {issuer: ca, leaf: Leaf{CommonName: "relay-1", DNSNames: []string{PeeringName}, Validity: -time.Minute}, wantChain: 1, wantErrText: "isn't valid now"},
		"for client authentication only": {issuer: ca, leaf: Leaf{CommonName: "relay-1", DNSNames: []string{PeeringName}, Validity: time.Hour, ExtKeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}, wantChain: 1, wantErrText: "server authentication"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cert, err := tt.issuer.Issue(tt.leaf)
			require.NoError(t, err)

			// The relay trusts the root alone, as CA_FILE holds it.
			_, err = relay.NewPeerTrust(ca.CertPEM(), &cert)

			assert.Equal(t, tt.leaf.CommonName, cert.Leaf.Subject.CommonName)
			assert.Len(t, cert.Certificate, tt.wantChain)
			if tt.wantErrText != "" {
				assert.ErrorContains(t, err, tt.wantErrText)
				return
			}
			assert.NoError(t, err, "the relay accepts it as its peer certificate")
		})
	}
}

func TestParseCA(t *testing.T) {
	ca, err := NewCA("test relay CA", time.Hour)
	require.NoError(t, err)
	keyPEM, err := ca.KeyPEM()
	require.NoError(t, err)

	tests := map[string]struct {
		certPEM, keyPEM []byte
		wantErrText     string
	}{
		"what CertPEM and KeyPEM wrote": {certPEM: ca.CertPEM(), keyPEM: keyPEM},
		"a certificate that isn't PEM":  {certPEM: []byte("nope"), keyPEM: keyPEM, wantErrText: "not a PEM certificate"},
		"a key that isn't PEM":          {certPEM: ca.CertPEM(), keyPEM: []byte("nope"), wantErrText: "not a PEM PKCS #8 private key"},
		"the certificate as the key":    {certPEM: ca.CertPEM(), keyPEM: ca.CertPEM(), wantErrText: "not a PEM PKCS #8 private key"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ParseCA(tt.certPEM, tt.keyPEM)

			if tt.wantErrText != "" {
				assert.ErrorContains(t, err, tt.wantErrText)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, ca.Cert.Raw, got.Cert.Raw)
			assert.True(t, ca.Key.Equal(got.Key), "the same key")
		})
	}
}

func TestCA_ValidFor(t *testing.T) {
	ca, err := NewCA("test relay CA", time.Hour)
	require.NoError(t, err)

	tests := map[string]struct {
		d    time.Duration
		want bool
	}{
		"within its validity": {d: 30 * time.Minute, want: true},
		"past its expiry":     {d: 2 * time.Hour},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := ca.ValidFor(tt.d)

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEncodePEM(t *testing.T) {
	ca, err := NewCA("test relay CA", time.Hour)
	require.NoError(t, err)
	intermediate, err := ca.NewIntermediate("test issuing CA", time.Hour)
	require.NoError(t, err)
	cert, err := intermediate.Issue(Leaf{CommonName: "relay-1", DNSNames: []string{PeeringName}, Validity: time.Hour})
	require.NoError(t, err)

	certPEM, keyPEM, err := EncodePEM(cert)

	require.NoError(t, err)
	loaded, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err, "the pair tls.LoadX509KeyPair reads")
	assert.Equal(t, cert.Certificate, loaded.Certificate, "the leaf and its chain")
}
