package relay

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testCA is a relay CA for a test.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
	pool *x509.CertPool
}

func newTestCA(tb testing.TB) testCA {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(tb, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test relay CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(tb, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(tb, err)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pool: pool}
}

// certSpec is what a test certificate carries.
type certSpec struct {
	cn       string
	dnsNames []string
	usages   []x509.ExtKeyUsage
	notAfter time.Time
}

// issue returns a certificate signed by the CA, as tls.Certificate with its
// Leaf set, and its PEM encodings.
func (ca testCA) issue(tb testing.TB, spec certSpec) (cert tls.Certificate, certPEM, keyPEM []byte) {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(tb, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	require.NoError(tb, err)
	notAfter := spec.notAfter
	if notAfter.IsZero() {
		notAfter = time.Now().Add(time.Hour)
	}
	usages := spec.usages
	if usages == nil {
		usages = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: spec.cn},
		NotBefore:    time.Now().Add(-2 * time.Hour),
		NotAfter:     notAfter,
		DNSNames:     spec.dnsNames,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  usages,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	require.NoError(tb, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(tb, err)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err = tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(tb, err)
	cert.Leaf, err = x509.ParseCertificate(der)
	require.NoError(tb, err)
	return cert, certPEM, keyPEM
}

// peerSpec is a relay's peer certificate.
func peerSpec(cn string) certSpec { return certSpec{cn: cn, dnsNames: []string{PeeringName}} }

// publicSpec is a relay's public server certificate, from another CA.
func publicSpec() certSpec {
	return certSpec{cn: "localhost", dnsNames: []string{"localhost"}, usages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
}

// writeFiles writes files into a fresh directory and returns it.
func writeFiles(tb testing.TB, files map[string][]byte) string {
	tb.Helper()
	dir := tb.TempDir()
	for name, data := range files {
		require.NoError(tb, os.WriteFile(filepath.Join(dir, name), data, 0o600))
	}
	return dir
}

func verifiedChain(leaf *x509.Certificate) *tls.ConnectionState {
	return &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{leaf}}, PeerCertificates: []*x509.Certificate{leaf}}
}

func TestClassify(t *testing.T) {
	ca := newTestCA(t)
	peer, _, _ := ca.issue(t, peerSpec("relay-1"))
	internal, _, _ := ca.issue(t, certSpec{cn: "egress-1"})
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

func TestIsSelf(t *testing.T) {
	ca := newTestCA(t)
	own, _, _ := ca.issue(t, peerSpec("relay-1"))
	other, _, _ := ca.issue(t, peerSpec("relay-2"))
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

func TestLoadPeerTrust(t *testing.T) {
	ca := newTestCA(t)
	otherCA := newTestCA(t)
	_, peerPEM, peerKey := ca.issue(t, peerSpec("relay-1"))
	_, foreignPEM, foreignKey := otherCA.issue(t, peerSpec("relay-x"))
	_, namelessPEM, namelessKey := ca.issue(t, certSpec{cn: "relay-1", dnsNames: []string{"relay-1.example"}})
	_, clientOnlyPEM, clientOnlyKey := ca.issue(t, certSpec{cn: "relay-1", dnsNames: []string{PeeringName}, usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	_, expiredPEM, expiredKey := ca.issue(t, certSpec{cn: "relay-1", dnsNames: []string{PeeringName}, notAfter: time.Now().Add(-time.Minute)})
	dir := writeFiles(t, map[string][]byte{
		"ca.crt": ca.pem, "peer.crt": peerPEM, "peer.key": peerKey,
		"foreign.crt": foreignPEM, "foreign.key": foreignKey,
		"nameless.crt": namelessPEM, "nameless.key": namelessKey,
		"clientonly.crt": clientOnlyPEM, "clientonly.key": clientOnlyKey,
		"expired.crt": expiredPEM, "expired.key": expiredKey,
	})
	// CA_FILE must be relative: the test runs from the directory.
	t.Chdir(dir)

	tests := map[string]struct {
		ca, cert, key string
		peers         bool
		wantCA        bool
		wantIdentity  bool
		wantErrText   string
	}{
		"standalone":                          {},
		"CA only":                             {ca: "ca.crt", wantCA: true},
		"CA and a peer identity":              {ca: "ca.crt", cert: "peer.crt", key: "peer.key", wantCA: true, wantIdentity: true},
		"full peering":                        {ca: "ca.crt", cert: "peer.crt", key: "peer.key", peers: true, wantCA: true, wantIdentity: true},
		"the certificate alone":               {ca: "ca.crt", cert: "peer.crt", wantErrText: "set together"},
		"the key alone":                       {ca: "ca.crt", key: "peer.key", wantErrText: "set together"},
		"a peer identity without a CA":        {cert: "peer.crt", key: "peer.key", wantErrText: "needs CA_FILE"},
		"PEERS without anything":              {peers: true, wantErrText: "PEERS needs"},
		"PEERS with the CA only":              {ca: "ca.crt", peers: true, wantErrText: "PEERS needs"},
		"a certificate from another CA":       {ca: "ca.crt", cert: "foreign.crt", key: "foreign.key", wantErrText: "isn't one CA_FILE issued"},
		"a certificate without the name":      {ca: "ca.crt", cert: "nameless.crt", key: "nameless.key", wantErrText: "peering name"},
		"a certificate for clients only":      {ca: "ca.crt", cert: "clientonly.crt", key: "clientonly.key", wantErrText: "server authentication"},
		"an expired certificate":              {ca: "ca.crt", cert: "expired.crt", key: "expired.key", wantErrText: "isn't valid now"},
		"a CA file that isn't there":          {ca: "nope.crt", wantErrText: "CA_FILE"},
		"a certificate file that isn't there": {ca: "ca.crt", cert: "nope.crt", key: "peer.key", wantErrText: "PEER_CERT_FILE"},
		"the key of another certificate":      {ca: "ca.crt", cert: "peer.crt", key: "foreign.key", wantErrText: "PEER_CERT_FILE"},
		"an absolute CA path":                 {ca: filepath.Join(dir, "ca.crt"), wantErrText: "relative"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			trust, err := LoadPeerTrust(tt.ca, tt.cert, tt.key, tt.peers)

			if tt.wantErrText != "" {
				assert.ErrorContains(t, err, tt.wantErrText)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantCA, trust.ca != nil)
			assert.Equal(t, tt.wantIdentity, trust.cert != nil)
			assert.Equal(t, tt.wantIdentity, len(trust.own) > 0)
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
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		s := tls.Server(conn, dialed)
		if err := s.Handshake(); err != nil {
			served <- result{err: err}
			return
		}
		st := s.ConnectionState()
		// Let a refused client learn it before the connection goes.
		_, _ = s.Write([]byte{0})
		served <- result{state: &st}
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(tb, err)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	c := tls.Client(conn, dialer)
	dialerErr = c.Handshake()
	if dialerErr == nil {
		// A client whose certificate the server refuses learns it here.
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
func TestPeerTrust_Handshakes(t *testing.T) {
	ca := newTestCA(t)
	otherCA := newTestCA(t)
	public, _, _ := otherCA.issue(t, publicSpec())
	own, _, _ := ca.issue(t, peerSpec("relay-1"))
	other, _, _ := ca.issue(t, peerSpec("relay-2"))
	internal, _, _ := ca.issue(t, certSpec{cn: "egress-1"})
	foreign, _, _ := otherCA.issue(t, peerSpec("relay-x"))
	withIdentity := &PeerTrust{ca: ca.pool, cert: &own, own: own.Certificate[0]}
	caOnly := &PeerTrust{ca: ca.pool}
	publicCfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{public}, NextProtos: []string{"h3", moqt.NextProtoMOQ}}
	// viaPublicName dials as a client of the relay's public name does.
	viaPublicName := func(protos []string, cert *tls.Certificate) *tls.Config {
		cfg := &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: protos, ServerName: "localhost", RootCAs: otherCA.pool}
		if cert != nil {
			cfg.Certificates = []tls.Certificate{*cert}
		}
		return cfg
	}
	native := []string{moqt.NextProtoMOQ}

	tests := map[string]struct {
		trust      *PeerTrust
		dialer     *tls.Config
		wantFail   bool
		wantServed *tls.Certificate // what the dialer was shown
		wantClass  sessionClass
		wantSelf   bool
	}{
		"a peer dials":           {trust: withIdentity, dialer: (&PeerTrust{ca: ca.pool, cert: &other}).DialerTLS(), wantServed: &own, wantClass: classPeer},
		"the relay dials itself": {trust: withIdentity, dialer: withIdentity.DialerTLS(), wantServed: &own, wantClass: classPeer, wantSelf: true},
		// A browser holds a certificate here, and is never asked for one.
		"a browser":                                    {trust: withIdentity, dialer: viaPublicName([]string{"h3"}, &internal), wantServed: &public, wantClass: classClient},
		"a native client with a credential":            {trust: withIdentity, dialer: viaPublicName(native, nil), wantServed: &public, wantClass: classClient},
		"an internal client, through the public name":  {trust: withIdentity, dialer: viaPublicName(native, &internal), wantServed: &public, wantClass: classInternal},
		"a peer certificate, through the public name":  {trust: withIdentity, dialer: viaPublicName(native, &other), wantServed: &public, wantClass: classPeer},
		"the peering name without a certificate":       {trust: withIdentity, dialer: &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: native, ServerName: PeeringName, RootCAs: ca.pool}, wantFail: true},
		"the peering name with a foreign certificate":  {trust: withIdentity, dialer: &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: native, ServerName: PeeringName, RootCAs: ca.pool, Certificates: []tls.Certificate{foreign}}, wantFail: true},
		"a peer dials a relay with no peer identity":   {trust: caOnly, dialer: (&PeerTrust{ca: ca.pool, cert: &other}).DialerTLS(), wantFail: true},
		"a peer dials a relay with no CA, no identity": {trust: &PeerTrust{}, dialer: (&PeerTrust{ca: ca.pool, cert: &other}).DialerTLS(), wantFail: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dialerState, dialedState, dialerErr, dialedErr := handshake(t, tt.dialer, tt.trust.ServerTLS(publicCfg))

			if tt.wantFail {
				assert.True(t, dialerErr != nil || dialedErr != nil, "the handshake must not complete on both sides")
				return
			}
			require.NoError(t, dialerErr)
			require.NoError(t, dialedErr)
			assert.Equal(t, tt.wantServed.Certificate[0], dialerState.PeerCertificates[0].Raw, "the certificate the dialer was shown")
			class, _ := classify(dialedState)
			assert.Equal(t, tt.wantClass, class)
			assert.Equal(t, tt.wantSelf, isSelf(dialedState, tt.trust.own))
			if tt.wantSelf {
				assert.True(t, isSelf(dialerState, tt.trust.own), "the dialing side sees it too")
			}
		})
	}
}

func TestSplitAddrList(t *testing.T) {
	tests := map[string]struct {
		raw  string
		want []string
	}{
		"empty":                {raw: "", want: nil},
		"one":                  {raw: "hub:4433", want: []string{"hub:4433"}},
		"list with whitespace": {raw: " hub1:4433 , hub2:4433 ,", want: []string{"hub1:4433", "hub2:4433"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := splitAddrList(tt.raw)

			assert.Equal(t, tt.want, got)
		})
	}
}
