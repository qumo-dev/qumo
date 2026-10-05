package relay

import (
	"bytes"
	"crypto/tls"
	"errors"
	"slices"

	"github.com/qumo-dev/gomoqt/moqt"
)

// Peer trust is mutual TLS on native QUIC: a relay that dials another shows
// its certificate as the client certificate, and the dialed relay decides
// from it whether the session is a trusted peer, which is served without a
// credential. There are two ways to decide.
//
// With a private CA (CA_FILE), the peer's certificate must verify against it.
// The TLS handshake does that, and a certificate it can't verify fails the
// handshake.
//
// Without one, relays that share a certificate are peers: a session is a peer
// when it presents this relay's own certificate. TLS has then proved that the
// other side holds the certificate's private key, and whoever holds that key
// can already stand in for this relay, so the rule trusts no one new. It fits
// a fleet that serves one wildcard certificate, and needs no setting besides
// the certificate itself.

// TrustSharedCertificate makes a session that presents this relay's own
// certificate a trusted peer. It asks native-QUIC clients for a certificate
// and compares the one they show with MOQServer's first certificate. Call it
// before the server starts, and not together with a private CA (ClientCAs).
func (s *Server) TrustSharedCertificate() error {
	cfg := s.MOQServer.TLSConfig
	if cfg == nil || len(cfg.Certificates) == 0 || len(cfg.Certificates[0].Certificate) == 0 {
		return errors.New("relay: no server certificate to share with peers")
	}
	s.peerCertificate = cfg.Certificates[0].Certificate[0]
	s.MOQServer.TLSConfig = requestPeerCertificate(cfg)
	return nil
}

// requestPeerCertificate returns server's config with native-QUIC clients
// asked for a certificate, which is not verified against any CA: isTrustedPeer
// compares it instead.
//
// A client that offers h3 is a browser or another WebTransport client. It is
// never asked: it could not be a peer, and a browser that holds client
// certificates would prompt its user to pick one.
func requestPeerCertificate(server *tls.Config) *tls.Config {
	native := server.Clone()
	native.ClientAuth = tls.RequestClientCert
	cfg := server.Clone()
	cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if !slices.Contains(hello.SupportedProtos, moqt.NextProtoMOQ) || slices.Contains(hello.SupportedProtos, "h3") {
			return nil, nil // the config as it is: no certificate asked for
		}
		return native, nil
	}
	return cfg
}

// isTrustedPeer reports whether a native-QUIC session is a relay peer.
//
// It is one when the handshake verified its client certificate against
// CA_FILE, or, with a shared certificate (own, this relay's leaf), when it
// presented exactly that certificate. The handshake proves the session holds
// the private key of the certificate it presents, whether or not a CA
// verified it.
func isTrustedPeer(state *tls.ConnectionState, own []byte) bool {
	if state == nil {
		return false
	}
	if len(state.VerifiedChains) > 0 {
		return true
	}
	return len(own) > 0 && len(state.PeerCertificates) > 0 && bytes.Equal(state.PeerCertificates[0].Raw, own)
}
