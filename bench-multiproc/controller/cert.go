package controller

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
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/qumo-dev/qumo/internal/devcert"
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

// peerCAValidity and peerCertValidity bound a cell's credentials: a cell is
// run for hours, and its directory may be reused for weeks.
const (
	peerCAValidity   = 30 * 24 * time.Hour
	peerCertValidity = 7 * 24 * time.Hour
)

// EnsurePeerCredentials writes the cell's relay CA, unless a usable one is in
// dir, and a peer certificate and key for every relay name.
func EnsurePeerCredentials(dir string, names []string) error {
	ca, err := loadOrCreatePeerCA(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, peerDir), 0o750); err != nil {
		return fmt.Errorf("mkdir peers: %w", err)
	}
	for _, name := range names {
		cert, err := ca.Issue(devcert.Leaf{CommonName: name, DNSNames: []string{relay.PeeringName}, Validity: peerCertValidity})
		if err != nil {
			return fmt.Errorf("issue the peer certificate for %s: %w", name, err)
		}
		certPEM, keyPEM, err := devcert.EncodePEM(cert)
		if err != nil {
			return fmt.Errorf("encode the peer certificate for %s: %w", name, err)
		}
		_, certPath, keyPath := PeerCredentialPaths(dir, name)
		if err := writeCredential(certPath, certPEM); err != nil {
			return err
		}
		if err := writeCredential(keyPath, keyPEM); err != nil {
			return err
		}
	}
	return nil
}

// loadOrCreatePeerCA returns the cell's relay CA from dir, creating it when
// its files are absent, or when the one there would expire before a
// certificate issued now does: a relay refuses a certificate from an expired
// CA, and the directory outlives the CA.
func loadOrCreatePeerCA(dir string) (*devcert.CA, error) {
	certPath, keyPath := filepath.Join(dir, peerCAFile), filepath.Join(dir, peerCAKey)
	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	switch {
	case certErr == nil && keyErr == nil:
		ca, err := devcert.ParseCA(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("the relay CA in %s: %w", dir, err)
		}
		if ca.ValidFor(peerCertValidity) {
			return ca, nil
		}
		// Expiring: replaced below, with every peer certificate after it.
	case errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist):
		// None yet.
	case certErr != nil && !errors.Is(certErr, os.ErrNotExist):
		return nil, fmt.Errorf("read %q: %w", certPath, certErr)
	case keyErr != nil && !errors.Is(keyErr, os.ErrNotExist):
		return nil, fmt.Errorf("read %q: %w", keyPath, keyErr)
	default:
		return nil, fmt.Errorf("one of %s and %s is missing: remove the other to make a new CA", certPath, keyPath)
	}
	ca, err := devcert.NewCA("qumo-benchctl relay CA", peerCAValidity)
	if err != nil {
		return nil, err
	}
	keyPEM, err = ca.KeyPEM()
	if err != nil {
		return nil, err
	}
	if err := writeCredential(certPath, ca.CertPEM()); err != nil {
		return nil, err
	}
	if err := writeCredential(keyPath, keyPEM); err != nil {
		return nil, err
	}
	return ca, nil
}

// writeCredential writes a PEM file readable by its owner alone: the relays
// that read it run as the same user.
func writeCredential(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	return nil
}
