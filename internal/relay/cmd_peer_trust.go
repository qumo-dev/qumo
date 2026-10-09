package relay

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The relay command's peer trust settings: CA_FILE, PEER_CERT_FILE and
// PEER_KEY_FILE, with PEERS. This file knows the settings and the files; what
// the trust they describe means is peer_trust.go.

// loadPeerTrust reads CA_FILE, PEER_CERT_FILE and PEER_KEY_FILE and checks
// they make a valid configuration, given whether PEERS is set:
//
//   - nothing set: a standalone relay, no session is a peer;
//   - CA_FILE alone: CA-issued certificates authenticate internal clients,
//     but the relay is no peer itself;
//   - CA_FILE with a peer certificate: the relay accepts peers;
//   - all of that with PEERS: full peering.
//
// It is an error to set only one of the peer files, a peer certificate
// without CA_FILE, or PEERS without CA_FILE and a peer certificate. There is
// no fallback to the public certificate (CERT_FILE): a relay without the two
// peer settings has no peer identity.
//
// The files are read once, here. A certificate renewed on disk is presented
// only after the relay restarts, so whatever renews it restarts the relay
// before the old one expires.
func loadPeerTrust(caFile, certFile, keyFile string, hasPeers bool) (*PeerTrust, error) {
	if (certFile == "") != (keyFile == "") {
		return nil, errors.New("PEER_CERT_FILE and PEER_KEY_FILE must be set together")
	}
	if certFile != "" && caFile == "" {
		return nil, errors.New("PEER_CERT_FILE needs CA_FILE: the CA that issued it, which peers are verified against")
	}
	if hasPeers && certFile == "" {
		return nil, errors.New("PEERS needs CA_FILE, PEER_CERT_FILE and PEER_KEY_FILE: a relay dials its peers with a certificate the CA issued")
	}
	caPEM, err := readCAFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load CA_FILE: %w", err)
	}
	var cert *tls.Certificate
	if certFile != "" {
		c, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load PEER_CERT_FILE: %w", err)
		}
		cert = &c
	}
	t, err := NewPeerTrust(caPEM, cert)
	if err != nil {
		return nil, fmt.Errorf("PEER_CERT_FILE, against CA_FILE: %w", err)
	}
	return t, nil
}

// readCAFile reads a PEM-encoded CA certificate file and checks it holds at
// least one certificate. Returns (nil, nil) when caFile is empty.
// CA_FILE must be a relative path that stays within the working directory,
// symlinks included.
func readCAFile(caFile string) ([]byte, error) {
	if caFile == "" {
		return nil, nil
	}
	if filepath.IsAbs(caFile) {
		return nil, fmt.Errorf("CA_FILE must be a relative path")
	}
	if !filepath.IsLocal(caFile) {
		return nil, fmt.Errorf("CA_FILE must not contain path traversal")
	}
	root, err := os.OpenRoot(".")
	if err != nil {
		return nil, fmt.Errorf("open the working directory for CA_FILE: %w", err)
	}
	defer root.Close()
	pemData, err := root.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA file %q: %w", caFile, err)
	}
	if !x509.NewCertPool().AppendCertsFromPEM(pemData) {
		return nil, fmt.Errorf("no valid certificates in CA file %q", caFile)
	}
	return pemData, nil
}
