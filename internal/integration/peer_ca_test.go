//go:build integration

package integration

import (
	"crypto/tls"
	"testing"
	"time"

	"github.com/qumo-dev/qumo/internal/devcert"
	"github.com/qumo-dev/qumo/internal/relay"
	"github.com/stretchr/testify/require"
)

// testCA is a relay CA for a test: it issues peer certificates (with the
// peering name) and internal clients' certificates (without).
type testCA struct {
	ca *devcert.CA
}

func newTestCA(tb testing.TB) *testCA {
	tb.Helper()
	ca, err := devcert.NewCA("test relay CA", time.Hour)
	require.NoError(tb, err)
	return &testCA{ca: ca}
}

// issue returns a certificate for identity signed by the CA, usable as a
// client and a server, carrying the peering name when peer is set.
func (ca *testCA) issue(tb testing.TB, identity string, peer bool) tls.Certificate {
	tb.Helper()
	leaf := devcert.Leaf{CommonName: identity, Validity: time.Hour}
	if peer {
		leaf.DNSNames = []string{relay.PeeringName}
	}
	cert, err := ca.ca.Issue(leaf)
	require.NoError(tb, err)
	return cert
}

// trust returns the relay's trust for this CA: with a peer identity when
// peerCert is set, or the CA alone.
func (ca *testCA) trust(tb testing.TB, peerCert *tls.Certificate) *relay.PeerTrust {
	tb.Helper()
	t, err := relay.NewPeerTrust(ca.ca.CertPEM(), peerCert)
	require.NoError(tb, err)
	return t
}
