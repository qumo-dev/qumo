package relay

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/qumo-dev/gomoqt/moqt"
)

// Relay peers authenticate each other with mutual TLS on native QUIC, under a
// CA of the operator's (CA_FILE). A relay's peer identity is a certificate
// that CA issued (PEER_CERT_FILE, PEER_KEY_FILE), separate from the public
// certificate it serves browsers (CERT_FILE, KEY_FILE): the two are different
// credentials with different trust.
//
// A relay that dials a peer (PEERS) presents its peer certificate as its
// client certificate, asks for PeeringName, and verifies the certificate the
// peer answers with against CA_FILE alone, so PEERS may name peers by any
// address. The dialed relay answers that name with its own peer certificate,
// to a native-QUIC client only, and verifies the client certificate against
// CA_FILE.
//
// What a CA-issued certificate grants depends on what it carries:
//
//   - with PeeringName among its DNS names, the session is a relay peer,
//     served without a credential and checked for nothing;
//   - without it, the session is an internal client (the HLS egress): it may
//     subscribe to anything and announce nothing. Only the CA can put the
//     name into a valid certificate.
//
// The certificate's subject common name is the relay's identity for logs and
// metrics; it is never interpreted. A relay that reaches itself through
// PEERS (a group name that resolves to it too) sees its own certificate and
// drops the session.

// PeeringName is the TLS server name a relay asks for when it dials a peer,
// and the DNS name a peer certificate must carry. It can never resolve.
const PeeringName = "peer.qumo.internal"

// sessionClass is what a native-QUIC session's TLS handshake made it.
type sessionClass int

const (
	// classClient presented no certificate the relay verified: it is admitted
	// by its credential like any client.
	classClient sessionClass = iota
	// classInternal presented a certificate CA_FILE verified, without
	// PeeringName: an internal client, subscribe-only.
	classInternal
	// classPeer presented a certificate CA_FILE verified that carries
	// PeeringName: a relay peer.
	classPeer
)

func (c sessionClass) String() string {
	switch c {
	case classInternal:
		return "internal client"
	case classPeer:
		return "peer"
	}
	return "client"
}

// PeerTrust is a relay's peer trust configuration: the CA it trusts, and its
// own peer identity when it has one. LoadPeerTrust reads it from files;
// ServerTLS and DialerTLS derive the listener's and the dialer's TLS
// configurations, and OwnCertificate is what Server.PeerCertificate takes.
type PeerTrust struct {
	// ca holds CA_FILE; nil when unset.
	ca *x509.CertPool
	// cert is the relay's peer certificate; nil when it has no peer identity.
	cert *tls.Certificate
	// own is cert's leaf in DER, for recognizing a session as this relay.
	own []byte
}

// LoadPeerTrust reads CA_FILE, PEER_CERT_FILE and PEER_KEY_FILE and checks
// they make a valid configuration, given whether PEERS is set:
//
//   - nothing set: a standalone relay, no session is a peer;
//   - CA_FILE alone: CA-issued certificates authenticate internal clients,
//     but the relay is no peer itself;
//   - CA_FILE with a peer certificate: the relay accepts peers;
//   - all of that with PEERS: full peering.
//
// It is an error to set only one of the peer files, a peer certificate
// without CA_FILE, or PEERS without CA_FILE and a peer certificate. A peer
// certificate must be one CA_FILE issued, carry PeeringName and be valid
// now: a relay that could never pass a peer's checks stops at startup rather
// than failing at every dial.
func LoadPeerTrust(caFile, certFile, keyFile string, hasPeers bool) (*PeerTrust, error) {
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
		return nil, fmt.Errorf("PEER_CERT_FILE: %w", err)
	}
	return t, nil
}

// NewPeerTrust returns the trust for a CA (PEM; nil for none) and a peer
// certificate (nil for none), checked as LoadPeerTrust checks files. It is
// for code that builds a Server itself, such as tests.
func NewPeerTrust(caPEM []byte, cert *tls.Certificate) (*PeerTrust, error) {
	t := &PeerTrust{}
	if caPEM != nil {
		t.ca = x509.NewCertPool()
		if !t.ca.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("no valid certificates in the CA")
		}
	}
	if cert == nil {
		return t, nil
	}
	if t.ca == nil {
		return nil, errors.New("a peer certificate needs a CA")
	}
	c := *cert
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return nil, err
	}
	c.Leaf = leaf
	if err := checkPeerCertificate(leaf, t.ca); err != nil {
		return nil, err
	}
	t.cert = &c
	t.own = c.Certificate[0]
	return t, nil
}

// checkPeerCertificate reports why leaf would fail a peer's checks: not
// issued by the CA, not for PeeringName, not usable as both a client and a
// server, or not valid now.
func checkPeerCertificate(leaf *x509.Certificate, ca *x509.CertPool) error {
	if !slices.Contains(leaf.DNSNames, PeeringName) {
		return fmt.Errorf("the certificate doesn't carry the peering name %q among its DNS names", PeeringName)
	}
	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: ca, DNSName: PeeringName, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
			return fmt.Errorf("the certificate isn't one CA_FILE issued for %s, or isn't valid now: %w", usageName(usage), err)
		}
	}
	return nil
}

func usageName(usage x509.ExtKeyUsage) string {
	if usage == x509.ExtKeyUsageClientAuth {
		return "client authentication"
	}
	return "server authentication"
}

// ServerTLS returns the listener's TLS configuration from the public one.
//
// With a CA, a native-QUIC client may present a certificate, verified against
// the CA when it does; one that asks for PeeringName is a relay dialing a
// peer, answered with the peer certificate, and must present one. A client
// that offers h3 is a browser or another WebTransport client: it is never
// asked for a certificate, since it could be neither a peer nor an internal
// client, and a browser holding certificates would prompt its user.
//
// Without a peer identity the relay keeps answering PeeringName with its
// public certificate, which the dialer refuses: a relay never pretends to be
// a peer it isn't.
func (t *PeerTrust) ServerTLS(public *tls.Config) *tls.Config {
	cfg := public.Clone()
	if t.ca == nil {
		return cfg
	}
	// Native QUIC: a certificate is optional and verified when given.
	native := public.Clone()
	native.ClientAuth = tls.VerifyClientCertIfGiven
	native.ClientCAs = t.ca
	// A relay dialing a peer: the peer certificate, and one required back.
	var peer *tls.Config
	if t.cert != nil {
		peer = native.Clone()
		peer.Certificates = []tls.Certificate{*t.cert}
		peer.NextProtos = []string{moqt.NextProtoMOQ}
		peer.ClientAuth = tls.RequireAndVerifyClientCert
	}
	cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		switch {
		case slices.Contains(hello.SupportedProtos, "h3") || !slices.Contains(hello.SupportedProtos, moqt.NextProtoMOQ):
			return nil, nil // the public config as it is: nothing asked for
		case hello.ServerName == PeeringName && peer != nil:
			return peer, nil
		}
		return native, nil
	}
	return cfg
}

// DialerTLS returns the configuration for dialing peers: the peer certificate
// as the client certificate, PeeringName as the server name, and the CA as
// the only root. It needs a peer identity.
func (t *PeerTrust) DialerTLS() *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{*t.cert},
		NextProtos:   []string{moqt.NextProtoMOQ},
		ServerName:   PeeringName,
		RootCAs:      t.ca,
	}
}

// OwnCertificate returns the relay's peer certificate in DER, or nil without
// a peer identity: the value for Server.PeerCertificate.
func (t *PeerTrust) OwnCertificate() []byte { return t.own }

// classify reports what a session's handshake made it, and the identity its
// certificate carries (the subject's common name, "" for a client).
func classify(state *tls.ConnectionState) (sessionClass, string) {
	if state == nil || len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
		return classClient, ""
	}
	leaf := state.VerifiedChains[0][0]
	if slices.Contains(leaf.DNSNames, PeeringName) {
		return classPeer, leaf.Subject.CommonName
	}
	return classInternal, leaf.Subject.CommonName
}

// isSelf reports whether the session's peer presented this relay's own peer
// certificate (own, DER): the relay reached itself.
func isSelf(state *tls.ConnectionState, own []byte) bool {
	return state != nil && len(own) > 0 && len(state.PeerCertificates) > 0 && bytes.Equal(state.PeerCertificates[0].Raw, own)
}

// readCAFile reads a PEM-encoded CA certificate file and checks it holds at
// least one certificate. Returns (nil, nil) when caFile is empty.
// CA_FILE must be a relative path with no path traversal components.
func readCAFile(caFile string) ([]byte, error) {
	if caFile == "" {
		return nil, nil
	}
	if filepath.IsAbs(caFile) {
		return nil, fmt.Errorf("CA_FILE must be a relative path")
	}
	caFile = filepath.Clean(caFile)
	if caFile == ".." || strings.HasPrefix(caFile, ".."+string(filepath.Separator)) || strings.Contains(caFile, string(filepath.Separator)+".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("CA_FILE must not contain path traversal")
	}
	pemData, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA file %q: %w", caFile, err)
	}
	if !x509.NewCertPool().AppendCertsFromPEM(pemData) {
		return nil, fmt.Errorf("no valid certificates in CA file %q", caFile)
	}
	return pemData, nil
}
