package relay

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/reqsign"
	"github.com/qumo-dev/qumo/internal/tlsclient"
)

// Relay peers authenticate each other with mutual TLS on native QUIC, under a
// CA of the operator's. A relay's peer identity is a certificate that CA
// issued, separate from the public certificate it serves browsers: the two
// are different credentials with different trust.
//
// A relay that dials a peer presents its peer certificate as its client
// certificate, asks for PeeringName, and verifies the certificate the peer
// answers with against the CA alone, so a peer may be dialed at any address.
// The dialed relay answers that name with its own peer certificate, to a
// native-QUIC client only, and verifies the client certificate against the
// CA.
//
// A CA-issued certificate with PeeringName among its DNS names makes the
// session a relay peer, served without a credential and checked for nothing.
// Only the CA can put the name into a valid certificate. One without the name
// grants nothing: its session is a client's, admitted by its credential.
//
// The certificate's subject common name is the relay's identity for logs and
// metrics; it is never interpreted. A relay that reaches itself among the
// peers it dials sees its own certificate and drops the session.
//
// This file is the trust itself: what a peer is, and the TLS configurations
// and session classes that follow. Which settings and files it comes from is
// the relay command's business (cmd_peer_trust.go).

// PeeringName is the TLS server name a relay asks for when it dials a peer,
// and the DNS name a peer certificate must carry. It can never resolve.
const PeeringName = tlsclient.PeeringName

// sessionClass is whom the relay takes a session for, which decides what it
// asks of the session and what it lets it do. A native-QUIC session's TLS
// handshake decides it; a session that came through an HTTP upgrade is a
// client.
type sessionClass int

const (
	// classClient presented no certificate the CA verified that carries
	// PeeringName: it is admitted by its credential like any client.
	classClient sessionClass = iota
	// classPeer presented a certificate the CA verified that carries
	// PeeringName: a relay peer.
	classPeer
)

// String is the class as it is logged and as a metric label.
func (c sessionClass) String() string {
	if c == classPeer {
		return "peer"
	}
	return "client"
}

// PeerTrust is a relay's peer trust: the CA it trusts, and its own peer
// identity when it has one. ServerTLS and DialerTLS derive the listener's and
// the dialer's TLS configurations from it, and OwnCertificate is what
// Server.PeerCertificate takes. The relay command swaps in a new PeerTrust
// when its files are renewed, and builds its configurations with serverTLS
// and dialerTLS, which ask for the current one at each handshake.
type PeerTrust struct {
	// ca is the CA; nil without one.
	ca *x509.CertPool
	// cert is the relay's peer certificate; nil when it has no peer identity.
	cert *tls.Certificate
	// own is cert's leaf in DER, for recognizing a session as this relay.
	own []byte
}

// NewPeerTrust returns the trust for a CA (PEM; nil for none) and a peer
// certificate (nil for none). The certificate must be one the CA issued,
// directly or through the intermediates that follow it in its chain, carry
// PeeringName, be usable as both a client and a server, and be valid now: a
// relay that could never pass a peer's checks is refused here rather than at
// every dial.
//
// A PeerTrust never changes: a CA or a certificate renewed on disk makes a
// new one, which the relay command swaps in (cert_reload.go).
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
	// What follows the leaf in the certificate is its chain: a peer sees
	// those certificates in the handshake and builds its path through
	// them, so the check here does too.
	intermediates := x509.NewCertPool()
	for _, der := range c.Certificate[1:] {
		ic, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("a certificate after the first isn't valid: %w", err)
		}
		intermediates.AddCert(ic)
	}
	if err := checkPeerCertificate(leaf, intermediates, t.ca); err != nil {
		return nil, err
	}
	t.cert = &c
	t.own = c.Certificate[0]
	return t, nil
}

// requestSigner returns the signer for the relay's requests to its control
// plane (qumo-dev/qumo#601), made from the peer identity t already holds, so
// that the relay signs with the very certificate it peers with; nil when it
// has none. A peer identity whose key can't sign what the control plane
// verifies (not ECDSA) is an error when the relay calls one
// (callsControlPlane), as it is for qumo dns: a relay that went on unsigned
// would be locked out once the control plane takes signatures alone.
// Without one the identity only peers, and the relay says so and carries on.
func requestSigner(t *PeerTrust, callsControlPlane bool) (*reqsign.Signer, error) {
	if t == nil || t.cert == nil {
		return nil, nil
	}
	signer, err := reqsign.New(*t.cert)
	if err == nil {
		return signer, nil
	}
	if callsControlPlane {
		return nil, fmt.Errorf("PEER_CERT_FILE, PEER_KEY_FILE: the peer identity can't sign the control-plane requests: %w", err)
	}
	slog.Warn("relay: the peer identity can't sign control-plane requests; there are none to sign", "err", err)
	return nil, nil
}

// checkPeerCertificate reports why leaf would fail a peer's checks: not
// issued by the CA (directly, or through intermediates), not for
// PeeringName, not usable as both a client and a server, or not valid now.
func checkPeerCertificate(leaf *x509.Certificate, intermediates, ca *x509.CertPool) error {
	if !slices.Contains(leaf.DNSNames, PeeringName) {
		return fmt.Errorf("the certificate doesn't carry the peering name %q among its DNS names", PeeringName)
	}
	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: ca, Intermediates: intermediates, DNSName: PeeringName, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
			return fmt.Errorf("the certificate isn't one the CA issued for %s, or isn't valid now: %w", usageName(usage), err)
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
// asked for a certificate, since it could not be a peer, and a browser
// holding certificates would prompt its user.
//
// Without a peer identity the relay keeps answering PeeringName with its
// public certificate, which the dialer refuses: a relay never pretends to be
// a peer it isn't.
//
// The configuration holds t for good: it does not follow certificate
// reloads. The relay command uses serverTLS with its reloader instead.
func (t *PeerTrust) ServerTLS(public *tls.Config) *tls.Config {
	return serverTLS(public, func() *PeerTrust { return t })
}

// serverTLS is ServerTLS for the trust current returns at each handshake,
// so that a renewed CA or peer certificate takes effect for the next one.
// Whether there is a CA at all is current's answer now: a relay's CA can be
// renewed, never added or taken away.
func serverTLS(public *tls.Config, current func() *PeerTrust) *tls.Config {
	cfg := public.Clone()
	if !current().HasCA() {
		return cfg
	}
	base := public.Clone()
	cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if slices.Contains(hello.SupportedProtos, "h3") || !slices.Contains(hello.SupportedProtos, moqt.NextProtoMOQ) {
			return nil, nil // the public config as it is: nothing asked for
		}
		t := current()
		// Native QUIC: a certificate is optional and verified when given.
		native := base.Clone()
		native.ClientAuth = tls.VerifyClientCertIfGiven
		native.ClientCAs = t.ca
		if hello.ServerName != PeeringName || t.cert == nil {
			return native, nil
		}
		// A relay dialing a peer: the peer certificate, and one required
		// back. GetCertificate would take precedence over Certificates for
		// a client that names a server, and answers with the public one.
		native.GetCertificate = nil
		native.Certificates = []tls.Certificate{*t.cert}
		native.NextProtos = []string{moqt.NextProtoMOQ}
		native.ClientAuth = tls.RequireAndVerifyClientCert
		return native, nil
	}
	return cfg
}

// DialerTLS returns the configuration for dialing peers: the peer certificate
// as the client certificate, PeeringName as the server name, and the CA as
// the only root. It needs a peer identity.
//
// The configuration holds t for good: it does not follow certificate
// reloads. The relay command uses dialerTLS with its reloader instead.
func (t *PeerTrust) DialerTLS() *tls.Config {
	return dialerTLS(func() *PeerTrust { return t })
}

// dialerTLS is DialerTLS for the trust current returns at each handshake.
// crypto/tls fixes RootCAs with the configuration, so the peer's certificate
// is verified in VerifyConnection instead, against the CA current then, as
// crypto/tls would have: the chain the peer sent, PeeringName, a server
// certificate, valid now. It runs on every handshake, resumed ones too.
func dialerTLS(current func() *PeerTrust) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{moqt.NextProtoMOQ},
		ServerName: PeeringName,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			if c := current().cert; c != nil {
				return c, nil
			}
			return &tls.Certificate{}, nil // none to send
		},
		InsecureSkipVerify: true, // verified by VerifyConnection, against the current CA
		VerifyConnection: func(state tls.ConnectionState) error {
			return current().verifyDialed(state.PeerCertificates)
		},
	}
}

// verifyDialed checks the certificates a dialed peer presented, leaf first,
// against the CA: issued by it for PeeringName, for a server, valid now.
func (t *PeerTrust) verifyDialed(certs []*x509.Certificate) error {
	if t.ca == nil {
		return errors.New("tls: no CA to verify the peer against")
	}
	if len(certs) == 0 {
		return errors.New("tls: the peer presented no certificate")
	}
	intermediates := x509.NewCertPool()
	for _, c := range certs[1:] {
		intermediates.AddCert(c)
	}
	opts := x509.VerifyOptions{Roots: t.ca, Intermediates: intermediates, DNSName: PeeringName, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if _, err := certs[0].Verify(opts); err != nil {
		return fmt.Errorf("tls: failed to verify the peer's certificate: %w", err)
	}
	return nil
}

// HasCA reports whether there is a CA: without one no session is a peer.
func (t *PeerTrust) HasCA() bool { return t.ca != nil }

// Identity returns the subject common name of the relay's peer certificate,
// what its peers log it as, or "" without a peer identity.
func (t *PeerTrust) Identity() string {
	if t.cert == nil {
		return ""
	}
	return t.cert.Leaf.Subject.CommonName
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
	return classClient, ""
}

// dialedIdentity returns the identity of a peer this relay dialed: the
// subject common name of the certificate it presented, which the dialer
// verified (dialerTLS), or "" without one.
func dialedIdentity(state *tls.ConnectionState) string {
	if state == nil || len(state.PeerCertificates) == 0 {
		return ""
	}
	return state.PeerCertificates[0].Subject.CommonName
}

// isSelf reports whether the session's peer presented this relay's own peer
// certificate (own, DER): the relay reached itself.
func isSelf(state *tls.ConnectionState, own []byte) bool {
	return state != nil && len(own) > 0 && len(state.PeerCertificates) > 0 && bytes.Equal(state.PeerCertificates[0].Raw, own)
}
