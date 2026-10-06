package controller

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/qumo-dev/qumo/internal/relay"
)

// CertPaths holds the PEM file paths for a generated self-signed certificate.
type CertPaths struct {
	Cert string
	Key  string
}

const (
	defaultCertFile = "cert.pem"
	defaultKeyFile  = "key.pem"
)

// EnsureCerts generates a self-signed ECDSA certificate in dir if either
// cert.pem or key.pem are missing. Returns the paths to the cert and key files.
func EnsureCerts(dir string) (*CertPaths, error) {
	certPath := filepath.Join(dir, defaultCertFile)
	keyPath := filepath.Join(dir, defaultKeyFile)

	// Check if both files already exist.
	if _, err := os.Stat(certPath); err == nil {
		if _, err := os.Stat(keyPath); err == nil {
			return &CertPaths{Cert: certPath, Key: keyPath}, nil
		}
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir %q: %w", dir, err)
	}

	// Generate ECDSA P-256 key pair.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}

	// Build a self-signed CA certificate.
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: "qumo-benchctl",
		},
		NotBefore: now.Add(-1 * time.Minute),
		NotAfter:  now.Add(7 * 24 * time.Hour),

		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},

		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}

	// Write cert PEM.
	certFile, err := os.Create(certPath)
	if err != nil {
		return nil, fmt.Errorf("create %q: %w", certPath, err)
	}
	defer certFile.Close()
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		return nil, fmt.Errorf("encode cert PEM: %w", err)
	}

	// Write key PEM (PKCS8).
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}
	keyFile, err := os.Create(keyPath)
	if err != nil {
		return nil, fmt.Errorf("create %q: %w", keyPath, err)
	}
	defer keyFile.Close()
	if err := pem.Encode(keyFile, &pem.Block{Type: "PRIVATE KEY", Bytes: kder}); err != nil {
		return nil, fmt.Errorf("encode key PEM: %w", err)
	}

	return &CertPaths{Cert: certPath, Key: keyPath}, nil
}

// Peer credentials: the relays of a cell authenticate each other with mutual
// TLS under a CA (the relay's CA_FILE, PEER_CERT_FILE and PEER_KEY_FILE;
// internal/relay/peer_trust.go). The cell gets a CA of its own, kept across
// runs, and each relay a certificate the CA issued, carrying the peering
// name and the relay's name as its identity.
const (
	peerCAFile = "peer-ca.crt"
	peerCAKey  = "peer-ca.key"
	peerDir    = "peers"
)

// PeerCredentialPaths locates a relay's peer credentials under dir. CA is
// relative to dir, since the relay runs there and CA_FILE must be relative.
func PeerCredentialPaths(dir, name string) (ca, cert, key string) {
	return peerCAFile, filepath.Join(dir, peerDir, name+".crt"), filepath.Join(dir, peerDir, name+".key")
}

// EnsurePeerCredentials writes the cell's relay CA, unless one is in dir, and
// a peer certificate and key for every relay name.
func EnsurePeerCredentials(dir string, names []string) error {
	caCert, caKey, err := loadOrCreatePeerCA(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, peerDir), 0o750); err != nil {
		return fmt.Errorf("mkdir peers: %w", err)
	}
	for _, name := range names {
		if err := issuePeerCertificate(dir, caCert, caKey, name); err != nil {
			return fmt.Errorf("issue the peer certificate for %s: %w", name, err)
		}
	}
	return nil
}

func loadOrCreatePeerCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPath, keyPath := filepath.Join(dir, peerCAFile), filepath.Join(dir, peerCAKey)
	if certPEM, err := os.ReadFile(certPath); err == nil {
		keyPEM, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, nil, fmt.Errorf("%s without %s: remove it to make a new CA", certPath, keyPath)
		}
		block, _ := pem.Decode(certPEM)
		if block == nil {
			return nil, nil, fmt.Errorf("%s: not PEM", certPath)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", certPath, err)
		}
		block, _ = pem.Decode(keyPEM)
		if block == nil {
			return nil, nil, fmt.Errorf("%s: not PEM", keyPath)
		}
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", keyPath, err)
		}
		key, ok := parsed.(*ecdsa.PrivateKey)
		if !ok {
			return nil, nil, fmt.Errorf("%s: not an ECDSA key", keyPath)
		}
		return cert, key, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "qumo-benchctl relay CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(30 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create CA certificate: %w", err)
	}
	if err := writePEMPair(certPath, keyPath, der, key); err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func issuePeerCertificate(dir string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, name string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(7 * 24 * time.Hour),
		DNSNames:     []string{relay.PeeringName},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}
	_, certPath, keyPath := PeerCredentialPaths(dir, name)
	return writePEMPair(certPath, keyPath, der, key)
}

// writePEMPair writes a certificate (DER) and its key (PKCS8) as PEM files,
// the key owner-only.
func writePEMPair(certPath, keyPath string, der []byte, key *ecdsa.PrivateKey) error {
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil { //nolint:gosec // a certificate is public
		return fmt.Errorf("write %q: %w", certPath, err)
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal private key: %w", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		return fmt.Errorf("write %q: %w", keyPath, err)
	}
	return nil
}
