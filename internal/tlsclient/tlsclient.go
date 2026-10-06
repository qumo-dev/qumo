// Package tlsclient holds the shared TLS configuration for qumo's
// relay-dialing clients: the HLS egress, the load generator, the latency probe,
// and the smoke test.
//
// All of them verify the relay's certificate by default and accept either a PEM
// trust anchor (--ca / RELAY_CA_FILE) or an explicit --insecure /
// RELAY_TLS_INSECURE=true escape hatch. A client that should be a trusted relay
// peer also presents a client certificate ([ApplyClientCert]). Centralizing that decision here means a
// future policy change — mTLS, SNI pinning, certificate-expiry checks, a new
// minimum TLS version — lands in one place rather than across every client.
package tlsclient

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// Apply configures tc to trust the relay: verify against the PEM trust anchor in
// caFile, or skip verification entirely when insecure (the explicit dev escape
// hatch). insecure dominates a caFile, matching crypto/tls, where
// InsecureSkipVerify short-circuits verification regardless of RootCAs — so a
// bad caFile path is never even read when insecure is set.
//
// Apply touches only tc.RootCAs and tc.InsecureSkipVerify. The caller owns the
// rest of the config (NextProtos, MinVersion, ...), because those vary per
// client: the load generator pins the MoQ ALPN, the egress leaves it to the
// dialer default, and so on. With neither caFile nor insecure set, Apply leaves
// RootCAs nil, which means crypto/tls verifies against the system root store.
func Apply(tc *tls.Config, caFile string, insecure bool) error {
	if insecure {
		tc.InsecureSkipVerify = true
		return nil
	}
	pool, err := loadCAPool(caFile)
	if err != nil {
		return err
	}
	tc.RootCAs = pool
	return nil
}

// loadCAPool reads a PEM cert file into a fresh pool. The relay's self-signed
// cert is its own issuer, so passing the cert itself is sufficient to trust it;
// this does not seed the system root store. An empty caFile returns a nil pool
// (no error), leaving the caller free to verify against the system roots.
func loadCAPool(caFile string) (*x509.CertPool, error) {
	if caFile == "" {
		return nil, nil
	}
	pemCert, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA cert %q: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemCert) {
		return nil, fmt.Errorf("no certificates found in %q", caFile)
	}
	return pool, nil
}

// ApplyClientCert makes tc present the certificate and key in certFile and
// keyFile, both PEM, as this client's TLS identity. A relay that verifies it
// against its CA_FILE serves the session as an internal client (subscribe
// only), over native QUIC only. With both empty it leaves tc unchanged; one without the other is an
// error.
func ApplyClientCert(tc *tls.Config, certFile, keyFile string) error {
	if certFile == "" && keyFile == "" {
		return nil
	}
	if certFile == "" || keyFile == "" {
		return errors.New("a client certificate needs both a cert file and a key file")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("load client certificate: %w", err)
	}
	tc.Certificates = []tls.Certificate{cert}
	return nil
}
