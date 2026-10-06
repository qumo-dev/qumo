package relay

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/devcert"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testCA returns a relay CA for a test.
func testCA(tb testing.TB) *devcert.CA {
	tb.Helper()
	ca, err := devcert.NewCA("test relay CA", time.Hour)
	require.NoError(tb, err)
	return ca
}

// issue returns a certificate ca issued for leaf, valid for an hour unless
// leaf says otherwise.
func issue(tb testing.TB, ca *devcert.CA, leaf devcert.Leaf) tls.Certificate {
	tb.Helper()
	if leaf.Validity == 0 {
		leaf.Validity = time.Hour
	}
	cert, err := ca.Issue(leaf)
	require.NoError(tb, err)
	return cert
}

// peerLeaf is a relay's peer certificate.
func peerLeaf(cn string) devcert.Leaf {
	return devcert.Leaf{CommonName: cn, DNSNames: []string{PeeringName}}
}

// rootsOf returns a pool trusting ca alone.
func rootsOf(ca *devcert.CA) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	return pool
}

func verifiedChain(leaf *x509.Certificate) *tls.ConnectionState {
	return &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{leaf}}, PeerCertificates: []*x509.Certificate{leaf}}
}

func Test_classify(t *testing.T) {
	ca := testCA(t)
	peer := issue(t, ca, peerLeaf("relay-1"))
	internal := issue(t, ca, devcert.Leaf{CommonName: "egress-1"})
	tests := map[string]struct {
		state        *tls.ConnectionState
		wantClass    sessionClass
		wantIdentity string
	}{
		"a peer certificate":                   {state: verifiedChain(peer.Leaf), wantClass: classPeer, wantIdentity: "relay-1"},
		"an internal client's certificate":     {state: verifiedChain(internal.Leaf), wantClass: classInternal, wantIdentity: "egress-1"},
		"a certificate the CA didn't verify":   {state: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{peer.Leaf}}, wantClass: classClient},
		"no certificate":                       {state: &tls.ConnectionState{}, wantClass: classClient},
		"no TLS state":                         {state: nil, wantClass: classClient},
		"a verified chain with no certificate": {state: &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{}}}, wantClass: classClient},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			class, identity := classify(tt.state)

			assert.Equal(t, tt.wantClass, class)
			assert.Equal(t, tt.wantIdentity, identity)
		})
	}
}

func Test_isSelf(t *testing.T) {
	ca := testCA(t)
	own := issue(t, ca, peerLeaf("relay-1"))
	other := issue(t, ca, peerLeaf("relay-2"))
	tests := map[string]struct {
		state *tls.ConnectionState
		own   []byte
		want  bool
	}{
		"the relay's own certificate":   {state: verifiedChain(own.Leaf), own: own.Certificate[0], want: true},
		"another relay's certificate":   {state: verifiedChain(other.Leaf), own: own.Certificate[0]},
		"no peer identity of its own":   {state: verifiedChain(own.Leaf)},
		"no certificate from the other": {state: &tls.ConnectionState{}, own: own.Certificate[0]},
		"no TLS state":                  {state: nil, own: own.Certificate[0]},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := isSelf(tt.state, tt.own)

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNewPeerTrust(t *testing.T) {
	ca := testCA(t)
	otherCA := testCA(t)
	issuing, err := ca.NewIntermediate("test issuing CA", time.Hour)
	require.NoError(t, err)
	peer := issue(t, ca, peerLeaf("relay-1"))
	// A leaf from an intermediate, with the intermediate after it in its
	// chain: what a PKI with its root offline hands out.
	chained := issue(t, issuing, peerLeaf("relay-1"))
	foreign := issue(t, otherCA, peerLeaf("relay-x"))
	nameless := issue(t, ca, devcert.Leaf{CommonName: "relay-1", DNSNames: []string{"relay-1.example"}})
	clientOnly := issue(t, ca, devcert.Leaf{CommonName: "relay-1", DNSNames: []string{PeeringName}, ExtKeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	expired := issue(t, ca, devcert.Leaf{CommonName: "relay-1", DNSNames: []string{PeeringName}, Validity: -time.Minute})

	tests := map[string]struct {
		caPEM        []byte
		cert         *tls.Certificate
		wantIdentity string
		wantErrText  string
	}{
		"nothing":                            {},
		"a CA alone":                         {caPEM: ca.CertPEM()},
		"a CA and its peer certificate":      {caPEM: ca.CertPEM(), cert: &peer, wantIdentity: "relay-1"},
		"a certificate from an intermediate": {caPEM: ca.CertPEM(), cert: &chained, wantIdentity: "relay-1"},
		"a CA that isn't PEM":                {caPEM: []byte("nope"), wantErrText: "no valid certificates in the CA"},
		"a certificate without a CA":         {cert: &peer, wantErrText: "needs a CA"},
		"a certificate from another CA":      {caPEM: ca.CertPEM(), cert: &foreign, wantErrText: "isn't one the CA issued"},
		"a certificate without the name":     {caPEM: ca.CertPEM(), cert: &nameless, wantErrText: "peering name"},
		"a certificate for clients only":     {caPEM: ca.CertPEM(), cert: &clientOnly, wantErrText: "server authentication"},
		"an expired certificate":             {caPEM: ca.CertPEM(), cert: &expired, wantErrText: "isn't valid now"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			trust, err := NewPeerTrust(tt.caPEM, tt.cert)

			if tt.wantErrText != "" {
				assert.ErrorContains(t, err, tt.wantErrText)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.caPEM != nil, trust.HasCA())
			assert.Equal(t, tt.wantIdentity, trust.Identity())
			assert.Equal(t, tt.cert != nil, trust.OwnCertificate() != nil)
		})
	}
}

// handshake runs a TLS handshake over a loopback TCP connection and returns
// both sides' states and handshake errors. Each side has a deadline, so a
// side that stops talking fails the other rather than hanging it.
func handshake(tb testing.TB, dialer, dialed *tls.Config) (dialerState, dialedState *tls.ConnectionState, dialerErr, dialedErr error) {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(tb, err)
	defer ln.Close()
	type result struct {
		state *tls.ConnectionState
		err   error
	}
	served := make(chan result, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			served <- result{err: err}
			return
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			served <- result{err: err}
			return
		}
		s := tls.Server(conn, dialed)
		if err := s.Handshake(); err != nil {
			served <- result{err: err}
			return
		}
		st := s.ConnectionState()
		// One byte, so that a client waits for the server's verdict on its
		// certificate; a failed write changes nothing the test reads.
		_, _ = s.Write([]byte{0})
		served <- result{state: &st}
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(tb, err)
	defer conn.Close()
	require.NoError(tb, conn.SetDeadline(time.Now().Add(5*time.Second)))
	c := tls.Client(conn, dialer)
	dialerErr = c.Handshake()
	if dialerErr == nil {
		// In TLS 1.3 the client finishes first: a server that refuses its
		// certificate says so in what the client reads next.
		if _, err := c.Read(make([]byte, 1)); err != nil {
			dialerErr = err
		} else {
			st := c.ConnectionState()
			dialerState = &st
		}
	}
	r := <-served
	return dialerState, r.state, dialerErr, r.err
}

// The listener's configuration answers the peering name with the peer
// certificate to native-QUIC clients, verifies client certificates against
// the CA, and leaves browsers alone.
func TestPeerTrust_ServerTLS(t *testing.T) {
	ca := testCA(t)
	otherCA := testCA(t)
	public := issue(t, otherCA, devcert.Leaf{CommonName: "localhost", DNSNames: []string{"localhost"}, ExtKeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	own := issue(t, ca, peerLeaf("relay-1"))
	other := issue(t, ca, peerLeaf("relay-2"))
	internal := issue(t, ca, devcert.Leaf{CommonName: "egress-1"})
	foreign := issue(t, otherCA, peerLeaf("relay-x"))
	caPool := rootsOf(ca)
	withIdentity := &PeerTrust{ca: caPool, cert: &own, own: own.Certificate[0]}
	caOnly := &PeerTrust{ca: caPool}
	asPeer := (&PeerTrust{ca: caPool, cert: &other}).DialerTLS()
	publicCfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{public}, NextProtos: []string{"h3", moqt.NextProtoMOQ}}
	native := []string{moqt.NextProtoMOQ}
	// viaPublicName dials as a client of the relay's public name does.
	viaPublicName := func(protos []string, cert *tls.Certificate) *tls.Config {
		cfg := &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: protos, ServerName: "localhost", RootCAs: rootsOf(otherCA)}
		if cert != nil {
			cfg.Certificates = []tls.Certificate{*cert}
		}
		return cfg
	}
	// viaPeeringName asks for the peering name as a relay does, with cert.
	viaPeeringName := func(cert *tls.Certificate) *tls.Config {
		cfg := &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: native, ServerName: PeeringName, RootCAs: caPool}
		if cert != nil {
			cfg.Certificates = []tls.Certificate{*cert}
		}
		return cfg
	}

	tests := map[string]struct {
		trust      *PeerTrust
		dialer     *tls.Config
		wantServed *tls.Certificate // what the dialer was shown
		wantClass  sessionClass
		wantSelf   bool
		// A refused handshake: what the side that refuses reports.
		wantDialedErr string
		wantDialerErr string
	}{
		"a peer dials":           {trust: withIdentity, dialer: asPeer, wantServed: &own, wantClass: classPeer},
		"the relay dials itself": {trust: withIdentity, dialer: withIdentity.DialerTLS(), wantServed: &own, wantClass: classPeer, wantSelf: true},
		// A browser holds a certificate here, and is never asked for one.
		"a browser":                                   {trust: withIdentity, dialer: viaPublicName([]string{"h3"}, &internal), wantServed: &public, wantClass: classClient},
		"a native client with a credential":           {trust: withIdentity, dialer: viaPublicName(native, nil), wantServed: &public, wantClass: classClient},
		"an internal client, through the public name": {trust: withIdentity, dialer: viaPublicName(native, &internal), wantServed: &public, wantClass: classInternal},
		"a peer certificate, through the public name": {trust: withIdentity, dialer: viaPublicName(native, &other), wantServed: &public, wantClass: classPeer},
		// The relay refuses: the peering name is for holders of a certificate from its CA.
		"the peering name without a certificate":      {trust: withIdentity, dialer: viaPeeringName(nil), wantDialedErr: "client didn't provide a certificate"},
		"the peering name with a foreign certificate": {trust: withIdentity, dialer: viaPeeringName(&foreign), wantDialedErr: "certificate signed by unknown authority"},
		// The dialer refuses: a relay with no peer identity answers with its public certificate.
		"a peer dials a relay with no peer identity":   {trust: caOnly, dialer: asPeer, wantDialerErr: "certificate is valid for localhost, not " + PeeringName},
		"a peer dials a relay with no CA, no identity": {trust: &PeerTrust{}, dialer: asPeer, wantDialerErr: "certificate is valid for localhost, not " + PeeringName},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dialerState, dialedState, dialerErr, dialedErr := handshake(t, tt.dialer, tt.trust.ServerTLS(publicCfg))

			switch {
			case tt.wantDialedErr != "":
				assert.ErrorContains(t, dialedErr, tt.wantDialedErr)
				assert.Error(t, dialerErr, "the dialer learns it was refused")
				return
			case tt.wantDialerErr != "":
				assert.ErrorContains(t, dialerErr, tt.wantDialerErr)
				return
			}
			require.NoError(t, dialerErr)
			require.NoError(t, dialedErr)
			assert.Equal(t, tt.wantServed.Certificate[0], dialerState.PeerCertificates[0].Raw, "the certificate the dialer was shown")
			class, _ := classify(dialedState)
			assert.Equal(t, tt.wantClass, class)
			assert.Equal(t, tt.wantSelf, isSelf(dialedState, tt.trust.own))
			if tt.wantSelf {
				// The dialer here is the relay itself, so its own certificate
				// is the one it was shown.
				assert.True(t, isSelf(dialerState, tt.trust.own), "the dialing side sees it too")
			}
		})
	}
}
