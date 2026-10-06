//go:build mage

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Peer credentials for development: a relay CA and one peer certificate per
// relay, as relays authenticate each other (qumo relay's CA_FILE,
// PEER_CERT_FILE and PEER_KEY_FILE). What a peer certificate must carry is
// fixed by the relay, in internal/relay/peer_trust.go:
//
//   - a DNS name equal to peeringName, which lets the dialing relay verify
//     it by server name and marks the holder as a relay peer (a CA-issued
//     certificate without it is an internal client, subscribe-only);
//   - a subject common name that is the relay's identity, for logs only;
//   - the client-authentication and server-authentication usages, since a
//     relay both dials and is dialed.
//
// Production credentials come from the operator's own CA; these are for the
// Compose topologies, the benchmarks and local runs.
const (
	// peeringName must match relay.PeeringName. magefiles is its own module,
	// so it cannot import the constant.
	peeringName = "peer.qumo.internal"
	peerCAFile  = "certs/peer-ca.crt"
	peerCAKey   = "certs/peer-ca.key"
	peerDir     = "certs/peers"
	// peerCAValidity and peerCertValidity are long: the CA is regenerated
	// whenever `mage cert` runs without one, and these never leave a
	// developer's machine.
	peerCAValidity   = 5 * 365 * 24 * time.Hour
	peerCertValidity = 365 * 24 * time.Hour
	// peerNamesEnv lists the relays to issue certificates for, comma-separated
	// (PEER_NAMES=relay-hub-asia,relay-edge-asia). Default: one relay.
	peerNamesEnv     = "PEER_NAMES"
	defaultPeerNames = "relay-1"
)

// peerCredentials writes the dev relay CA, unless one is there, and a peer
// certificate and key for every name in PEER_NAMES, replacing any there.
func peerCredentials() error {
	names := peerNames()
	caCert, caKey, err := loadOrCreatePeerCA()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(peerDir, 0o755); err != nil {
		return err
	}
	for _, name := range names {
		if err := issuePeerCertificate(caCert, caKey, name); err != nil {
			return fmt.Errorf("issue the peer certificate for %s: %w", name, err)
		}
	}
	fmt.Println()
	fmt.Println("🤝 Peer credentials (relay-to-relay mutual TLS):")
	fmt.Printf("   📄 %s  (CA_FILE on every relay)\n", peerCAFile)
	for _, name := range names {
		fmt.Printf("   🪪 %s/%s.crt + .key  (PEER_CERT_FILE/PEER_KEY_FILE of %s)\n", peerDir, name, name)
	}
	fmt.Printf("   Names come from %s (comma-separated); each relay needs one of its own.\n", peerNamesEnv)
	return nil
}

func peerNames() []string {
	raw := os.Getenv(peerNamesEnv)
	if raw == "" {
		raw = defaultPeerNames
	}
	var names []string
	for n := range strings.SplitSeq(raw, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// loadOrCreatePeerCA returns the dev relay CA, creating it when its files are
// absent. An existing CA is kept so that certificates issued earlier stay
// valid for relays that still hold them.
func loadOrCreatePeerCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, certErr := os.ReadFile(peerCAFile)
	keyPEM, keyErr := os.ReadFile(peerCAKey)
	switch {
	case certErr == nil && keyErr == nil:
		cert, err := parsePEMCertificate(certPEM)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", peerCAFile, err)
		}
		key, err := parsePEMKey(keyPEM)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", peerCAKey, err)
		}
		return cert, key, nil
	case errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist):
		return createPeerCA()
	case certErr != nil && !errors.Is(certErr, os.ErrNotExist):
		return nil, nil, certErr
	case keyErr != nil && !errors.Is(keyErr, os.ErrNotExist):
		return nil, nil, keyErr
	}
	return nil, nil, fmt.Errorf("one of %s and %s is missing; remove the other to make a new CA", peerCAFile, peerCAKey)
}

func createPeerCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "qumo dev relay CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(peerCAValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	if err := writeCertAndKey(peerCAFile, peerCAKey, der, key); err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func issuePeerCertificate(caCert *x509.Certificate, caKey *ecdsa.PrivateKey, name string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(peerCertValidity),
		DNSNames:     []string{peeringName},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	return writeCertAndKey(filepath.Join(peerDir, name+".crt"), filepath.Join(peerDir, name+".key"), der, key)
}

// writeCertAndKey writes a certificate (DER) and its key as PEM, the key
// owner-only.
func writeCertAndKey(certFile, keyFile string, der []byte, key *ecdsa.PrivateKey) error {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil { //nolint:gosec // a certificate is public
		return err
	}
	return os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
}

func parsePEMCertificate(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("not a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func parsePEMKey(data []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "EC PRIVATE KEY" {
		return nil, errors.New("not a PEM EC private key")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}
