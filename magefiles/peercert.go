//go:build mage

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qumo-dev/qumo/internal/devcert"
)

// Peer credentials for development: a relay CA and one peer certificate per
// relay, as relays authenticate each other (qumo relay's CA_FILE,
// PEER_CERT_FILE and PEER_KEY_FILE). devcert issues them, as it does for the
// benchmark and the tests, to what the relay requires of a peer certificate
// (internal/relay/peer_trust.go): the peering name among its DNS names, the
// relay's identity as its subject common name, and the client- and
// server-authentication usages.
//
// Production credentials come from the operator's own CA; these are for the
// Compose topologies and local runs.
const (
	peerCAFile = "certs/peer-ca.crt"
	peerCAKey  = "certs/peer-ca.key"
	peerDir    = "certs/peers"
	// A developer's CA and certificates never leave the machine, and are
	// reissued by running `mage cert` again.
	peerCAValidity   = 5 * 365 * 24 * time.Hour
	peerCertValidity = 365 * 24 * time.Hour
	// peerNamesEnv lists the relays to issue certificates for, comma-separated
	// (PEER_NAMES=relay-hub-asia,relay-edge-asia). Default: one relay.
	peerNamesEnv     = "PEER_NAMES"
	defaultPeerNames = "relay-1"
)

// peerCredentials writes the dev relay CA, unless a usable one is there, and
// a peer certificate and key for every name in PEER_NAMES, replacing any
// there.
func peerCredentials() error {
	names := peerNames()
	ca, err := loadOrCreatePeerCA()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(peerDir, 0o755); err != nil {
		return err
	}
	for _, name := range names {
		cert, err := ca.Issue(devcert.Leaf{CommonName: name, DNSNames: []string{devcert.PeeringName}, Validity: peerCertValidity})
		if err != nil {
			return fmt.Errorf("issue the peer certificate for %s: %w", name, err)
		}
		certPEM, keyPEM, err := devcert.EncodePEM(cert)
		if err != nil {
			return fmt.Errorf("encode the peer certificate for %s: %w", name, err)
		}
		if err := writeCredential(filepath.Join(peerDir, name+".crt"), certPEM); err != nil {
			return err
		}
		if err := writeCredential(filepath.Join(peerDir, name+".key"), keyPEM); err != nil {
			return err
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
// absent or when it would expire before a certificate issued now does. An
// existing CA with time left is kept, so that certificates issued earlier
// stay valid for relays that still hold them.
func loadOrCreatePeerCA() (*devcert.CA, error) {
	certPEM, certErr := os.ReadFile(peerCAFile)
	keyPEM, keyErr := os.ReadFile(peerCAKey)
	switch {
	case certErr == nil && keyErr == nil:
		ca, err := devcert.ParseCA(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("%s and %s: %w (remove both to make a new CA)", peerCAFile, peerCAKey, err)
		}
		if ca.ValidFor(peerCertValidity) {
			return ca, nil
		}
		// Expiring: replaced below, with every peer certificate after it.
	case errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist):
		// None yet.
	case certErr != nil && !errors.Is(certErr, os.ErrNotExist):
		return nil, certErr
	case keyErr != nil && !errors.Is(keyErr, os.ErrNotExist):
		return nil, keyErr
	default:
		return nil, fmt.Errorf("one of %s and %s is missing; remove the other to make a new CA", peerCAFile, peerCAKey)
	}
	ca, err := devcert.NewCA("qumo dev relay CA", peerCAValidity)
	if err != nil {
		return nil, err
	}
	keyPEM, err = ca.KeyPEM()
	if err != nil {
		return nil, err
	}
	if err := writeCredential(peerCAFile, ca.CertPEM()); err != nil {
		return nil, err
	}
	if err := writeCredential(peerCAKey, keyPEM); err != nil {
		return nil, err
	}
	return ca, nil
}

// writeCredential writes a PEM file readable by its owner alone: the relay
// that reads it runs as the same user.
func writeCredential(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
