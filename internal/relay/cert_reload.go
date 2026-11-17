package relay

import (
	"crypto/tls"
	"log/slog"
	"sync/atomic"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/certreload"
	"github.com/qumo-dev/qumo/internal/reqsign"
)

// The relay re-reads its certificate files while it runs, so that whatever
// renews them (cert-manager, a Vault or step-ca agent, a cron job) need not
// restart it: the public certificate (CERT_FILE, KEY_FILE), the peer
// certificate (PEER_CERT_FILE, PEER_KEY_FILE) and the relay CA (CA_FILE).
//
// Every certreload.Interval it reads them all and compares what they hold
// with what is in use; what changed is taken whole, and only once it checks
// out, as at startup: a certificate that matches its key, and a peer
// certificate the CA issued, for the peering name, valid now. Anything else
// is logged (once while the error stays the same) and the certificates in
// use are kept. The TLS configurations ask for the certificates at each
// handshake, so a new one is presented from the next handshake on. A session
// already established keeps its connection: TLS is not asked again on an
// established QUIC or TCP connection.

// certFiles names the files the relay's certificates are read from. The
// peer ones may be empty: no CA, or no peer identity.
type certFiles struct {
	cert, key         string // CERT_FILE, KEY_FILE
	ca                string // CA_FILE
	peerCert, peerKey string // PEER_CERT_FILE, PEER_KEY_FILE
}

// certReloader holds the certificates in use, swaps in renewed ones, and
// derives the TLS configurations that read them at every handshake.
type certReloader struct {
	files certFiles
	// public is CERT_FILE's certificate.
	public atomic.Pointer[tls.Certificate]
	// trust is the CA and the peer certificate.
	trust atomic.Pointer[PeerTrust]
	// signer signs control-plane requests with the peer certificate; nil
	// when it signs none.
	signer *reqsign.Signer
	// publicLog and peerLog log and count the readings of each.
	publicLog, peerLog *certreload.Cert
}

// newCertReloader returns a reloader holding the certificates the relay
// started with.
func newCertReloader(files certFiles, public tls.Certificate, trust *PeerTrust, signer *reqsign.Signer) *certReloader {
	r := &certReloader{
		files:     files,
		signer:    signer,
		publicLog: certreload.NewCert(slog.Default(), certMetrics, "public", &public),
		peerLog:   certreload.NewCert(slog.Default(), certMetrics, "peer", trust.cert),
	}
	r.public.Store(&public)
	r.trust.Store(trust)
	return r
}

// certificate returns the public certificate in use, for GetCertificate.
func (r *certReloader) certificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.public.Load(), nil
}

// currentTrust returns the peer trust in use.
func (r *certReloader) currentTrust() *PeerTrust { return r.trust.Load() }

// ownCertificate returns the peer certificate in use (DER), or nil: what
// Server.PeerCertificate takes.
func (r *certReloader) ownCertificate() []byte { return r.currentTrust().OwnCertificate() }

// publicTLS returns the listener's TLS configuration before peer trust:
// the public certificate, and the ALPNs of HTTP/3 (WebTransport) and native
// MoQ.
func (r *certReloader) publicTLS() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: r.certificate,
		NextProtos:     []string{"h3", moqt.NextProtoMOQ},
	}
}

// reload reads the certificate files once, and swaps in what changed and
// checks out.
func (r *certReloader) reload() {
	r.reloadPublic()
	if r.files.ca != "" {
		r.reloadTrust()
	}
}

func (r *certReloader) reloadPublic() {
	cert, err := tls.LoadX509KeyPair(r.files.cert, r.files.key)
	if err != nil {
		r.publicLog.Failed("CERT_FILE, KEY_FILE", err)
		return
	}
	if certreload.SameChain(&cert, r.public.Load()) {
		r.publicLog.Unchanged()
		return
	}
	r.public.Store(&cert)
	r.publicLog.Taken(&cert)
}

func (r *certReloader) reloadTrust() {
	caPEM, err := readCAFile(r.files.ca)
	if err != nil {
		r.peerLog.Failed("CA_FILE", err)
		return
	}
	var cert *tls.Certificate
	if r.files.peerCert != "" {
		c, err := tls.LoadX509KeyPair(r.files.peerCert, r.files.peerKey)
		if err != nil {
			r.peerLog.Failed("PEER_CERT_FILE, PEER_KEY_FILE", err)
			return
		}
		cert = &c
	}
	current := r.trust.Load()
	trust, err := NewPeerTrust(caPEM, cert)
	if err != nil {
		r.peerLog.Failed("PEER_CERT_FILE, against CA_FILE", err)
		return
	}
	newCA, newCert := !trust.ca.Equal(current.ca), !certreload.SameChain(trust.cert, current.cert)
	if !newCA && !newCert {
		r.peerLog.Unchanged()
		return
	}
	// The signer first: it refuses a key it can't sign with, and then
	// nothing has changed.
	if r.signer != nil && newCert {
		if err := r.signer.Rotate(*trust.cert); err != nil {
			r.peerLog.Failed("PEER_CERT_FILE, PEER_KEY_FILE", err)
			return
		}
	}
	r.trust.Store(trust)
	if newCert {
		r.peerLog.Taken(trust.cert)
	} else {
		r.peerLog.Taken(nil)
	}
	if newCA {
		slog.Info("relay: CA_FILE renewed: new peer handshakes are verified against it", "ca_file", r.files.ca)
	}
}
