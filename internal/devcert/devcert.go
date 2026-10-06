// Package devcert issues the certificates development and test setups need:
// a CA and leaf certificates under it. `mage cert`, the multi-process
// benchmark and the integration tests all use it, so that what they issue
// can't drift apart, or away from what the relay requires of a peer
// certificate (internal/relay/peer_trust.go).
//
// It is not for production: an operator's own CA issues those certificates.
package devcert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// PeeringName is the DNS name a relay's peer certificate carries. It equals
// relay.PeeringName, which TestPeeringNameMatchesRelay holds it to: this
// package stays free of the relay's dependencies so that the magefile can
// use it.
const PeeringName = "peer.qumo.internal"

// backdate covers clock differences between the machine that issues a
// certificate and the one that checks it.
const backdate = time.Minute

// CA is a certificate authority that issues leaf certificates.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
	// intermediate marks a CA another CA issued (NewIntermediate).
	intermediate bool
}

// NewCA creates a self-signed CA named commonName, valid for validity.
func NewCA(commonName string, validity time.Duration) (*CA, error) {
	return newCA(commonName, validity, nil)
}

// NewIntermediate creates a CA named commonName that this CA issued: what a
// PKI with its root kept offline signs leaf certificates with.
func (ca *CA) NewIntermediate(commonName string, validity time.Duration) (*CA, error) {
	return newCA(commonName, validity, ca)
}

// newCA creates a CA signed by parent, or by itself without one.
func newCA(commonName string, validity time.Duration, parent *CA) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate the CA key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(validity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	signerCert, signerKey := tmpl, key
	if parent != nil {
		signerCert, signerKey = parent.Cert, parent.Key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signerCert, &key.PublicKey, signerKey)
	if err != nil {
		return nil, fmt.Errorf("create the CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse the CA certificate: %w", err)
	}
	return &CA{Cert: cert, Key: key, intermediate: parent != nil}, nil
}

// ParseCA reads a CA from the PEM that CertPEM and KeyPEM wrote.
func ParseCA(certPEM, keyPEM []byte) (*CA, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("the CA certificate is not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse the CA certificate: %w", err)
	}
	block, _ = pem.Decode(keyPEM)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("the CA key is not a PEM PKCS #8 private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse the CA key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("the CA key is not an ECDSA key")
	}
	return &CA{Cert: cert, Key: key}, nil
}

// ValidFor reports whether the CA is valid now and stays so for d, which is
// what a certificate it issues for d needs.
func (ca *CA) ValidFor(d time.Duration) bool {
	now := time.Now()
	return !now.Before(ca.Cert.NotBefore) && now.Add(d).Before(ca.Cert.NotAfter)
}

// CertPEM returns the CA's certificate as PEM: a relay's CA_FILE.
func (ca *CA) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw})
}

// KeyPEM returns the CA's private key as PKCS #8 PEM.
func (ca *CA) KeyPEM() ([]byte, error) {
	return keyPEM(ca.Key)
}

// Leaf describes a certificate to issue.
type Leaf struct {
	// CommonName is the subject: for a peer certificate, the relay's
	// identity, which relays log and never interpret.
	CommonName string
	// DNSNames are the names the certificate is valid for. PeeringName
	// among them makes it a relay's peer certificate; without it, a
	// certificate from a relay's CA is an internal client's.
	DNSNames []string
	// Validity is how long the certificate is valid from now. Negative
	// issues one that has already expired, for a test of that refusal.
	Validity time.Duration
	// ExtKeyUsages overrides the usages; nil means client and server
	// authentication, which a relay's peer certificate needs since a relay
	// both dials and is dialed.
	ExtKeyUsages []x509.ExtKeyUsage
}

// Issue returns a certificate for l signed by the CA. Its chain carries the
// CA's own certificate when the CA is an intermediate, as a peer needs to
// see it to verify the leaf against the root.
func (ca *CA) Issue(l Leaf) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate the key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	usages := l.ExtKeyUsages
	if usages == nil {
		usages = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}
	}
	now := time.Now()
	notBefore := now.Add(-backdate)
	if l.Validity < 0 {
		// Already expired: the period still has to run forwards.
		notBefore = now.Add(l.Validity - backdate)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: l.CommonName},
		NotBefore:    notBefore,
		NotAfter:     now.Add(l.Validity),
		DNSNames:     l.DNSNames,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  usages,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create the certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse the certificate: %w", err)
	}
	chain := [][]byte{der}
	if ca.intermediate {
		chain = append(chain, ca.Cert.Raw)
	}
	return tls.Certificate{Certificate: chain, PrivateKey: key, Leaf: leaf}, nil
}

// EncodePEM returns cert's chain, leaf first, and its private key (PKCS #8)
// as PEM, the pair tls.LoadX509KeyPair reads. The key must be one Issue made.
func EncodePEM(cert tls.Certificate) (certPEM, keyPEMBytes []byte, err error) {
	if len(cert.Certificate) == 0 {
		return nil, nil, errors.New("no certificate to encode")
	}
	key, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, errors.New("the private key is not an ECDSA key")
	}
	keyPEMBytes, err = keyPEM(key)
	if err != nil {
		return nil, nil, err
	}
	for _, der := range cert.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	return certPEM, keyPEMBytes, nil
}

func keyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal the private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func newSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate a serial number: %w", err)
	}
	return serial, nil
}
