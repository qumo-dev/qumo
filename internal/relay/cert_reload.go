package relay

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/reqsign"
)

// The relay re-reads its certificate files while it runs, so that whatever
// renews them (cert-manager, a Vault or step-ca agent, a cron job) need not
// restart it: the public certificate (CERT_FILE, KEY_FILE), the peer
// certificate (PEER_CERT_FILE, PEER_KEY_FILE) and the relay CA (CA_FILE).
//
// Every certReloadInterval it reads them all and compares what they hold
// with what is in use; what changed is taken whole, and only once it checks
// out, as at startup: a certificate that matches its key, and a peer
// certificate the CA issued, for the peering name, valid now. Anything else
// is logged and the certificates in use are kept. The TLS configurations ask
// for the certificates at each handshake, so a new one is presented from the
// next handshake on. A session already established keeps its connection:
// TLS is not asked again on an established QUIC or TCP connection.

// certReloadInterval is how often the relay reads its certificate files.
const certReloadInterval = time.Minute

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
}

// newCertReloader returns a reloader holding the certificates the relay
// started with.
func newCertReloader(files certFiles, public tls.Certificate, trust *PeerTrust, signer *reqsign.Signer) *certReloader {
	r := &certReloader{files: files, signer: signer}
	r.public.Store(&public)
	r.trust.Store(trust)
	recordNotAfter("public", &public)
	recordNotAfter("peer", trust.cert)
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

// run reloads the certificates every certReloadInterval until ctx ends.
func (r *certReloader) run(ctx context.Context) {
	ticker := time.NewTicker(certReloadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reload()
		}
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
		certNotTaken("public", "CERT_FILE, KEY_FILE", err)
		return
	}
	if sameChain(&cert, r.public.Load()) {
		return
	}
	r.public.Store(&cert)
	metricCertReloads.WithLabelValues("public", "ok").Inc()
	certTaken("public", &cert)
}

func (r *certReloader) reloadTrust() {
	caPEM, err := readCAFile(r.files.ca)
	if err != nil {
		certNotTaken("peer", "CA_FILE", err)
		return
	}
	var cert *tls.Certificate
	if r.files.peerCert != "" {
		c, err := tls.LoadX509KeyPair(r.files.peerCert, r.files.peerKey)
		if err != nil {
			certNotTaken("peer", "PEER_CERT_FILE, PEER_KEY_FILE", err)
			return
		}
		cert = &c
	}
	current := r.trust.Load()
	trust, err := NewPeerTrust(caPEM, cert)
	if err != nil {
		certNotTaken("peer", "PEER_CERT_FILE, against CA_FILE", err)
		return
	}
	newCA, newCert := !trust.ca.Equal(current.ca), !sameChain(trust.cert, current.cert)
	if !newCA && !newCert {
		return
	}
	// The signer first: it refuses a key it can't sign with, and then
	// nothing has changed.
	if r.signer != nil && newCert {
		if err := r.signer.Rotate(*trust.cert); err != nil {
			certNotTaken("peer", "PEER_CERT_FILE, PEER_KEY_FILE", err)
			return
		}
	}
	r.trust.Store(trust)
	metricCertReloads.WithLabelValues("peer", "ok").Inc()
	if newCert {
		certTaken("peer", trust.cert)
	}
	if newCA {
		slog.Info("relay: CA_FILE renewed: new peer handshakes are verified against it", "ca_file", r.files.ca)
	}
}

// sameChain reports whether a and b hold the same certificates; nil for
// none.
func sameChain(a, b *tls.Certificate) bool {
	if a == nil || b == nil {
		return a == b
	}
	return slices.EqualFunc(a.Certificate, b.Certificate, slices.Equal)
}

// certTaken logs a renewed certificate (kind: public or peer) and records
// its expiry.
func certTaken(kind string, cert *tls.Certificate) {
	recordNotAfter(kind, cert)
	if cert.Leaf == nil {
		return
	}
	slog.Info("relay: certificate renewed: new handshakes present it", "cert", kind,
		"subject", cert.Leaf.Subject.CommonName, "serial", fmt.Sprintf("%X", cert.Leaf.SerialNumber), "not_after", cert.Leaf.NotAfter)
}

// certNotTaken logs and counts certificate files the relay couldn't take
// (kind: public or peer; settings: the settings that name them).
func certNotTaken(kind, settings string, err error) {
	metricCertReloads.WithLabelValues(kind, "error").Inc()
	slog.Error("relay: certificate files not taken; keeping the certificate in use", "cert", kind, "settings", settings, "err", err)
}

// recordNotAfter sets the expiry gauge for the certificate in use; nil
// (no peer identity) leaves it unset.
func recordNotAfter(kind string, cert *tls.Certificate) {
	if cert == nil || cert.Leaf == nil {
		return
	}
	metricCertNotAfter.WithLabelValues(kind).Set(float64(cert.Leaf.NotAfter.Unix()))
}
