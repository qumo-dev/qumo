package relay

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newServerCert returns a self-signed certificate for localhost, as a relay
// serves one. Each call makes a different certificate and key.
func newServerCert(tb testing.TB) tls.Certificate {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(tb, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	require.NoError(tb, err)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		// A public CA's certificate for a server: no client-auth usage.
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(tb, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// handshake runs a TLS handshake over an in-memory connection and returns the
// state the server saw.
func handshake(tb testing.TB, client, server *tls.Config) tls.ConnectionState {
	tb.Helper()
	clientConn, serverConn := net.Pipe()
	tb.Cleanup(func() { _ = clientConn.Close(); _ = serverConn.Close() })
	c, s := tls.Client(clientConn, client), tls.Server(serverConn, server)
	clientErr := make(chan error, 1)
	go func() { clientErr <- c.Handshake() }()
	require.NoError(tb, s.Handshake())
	require.NoError(tb, <-clientErr)
	return s.ConnectionState()
}

func TestIsTrustedPeer(t *testing.T) {
	own := []byte("this relay's certificate")
	tests := map[string]struct {
		state *tls.ConnectionState
		own   []byte
		want  bool
	}{
		"verified by CA_FILE":                   {state: &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}, want: true},
		"verified by CA_FILE, shared cert also": {state: &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}, own: own, want: true},
		"presents this relay's certificate":     {state: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: own}}}, own: own, want: true},
		"presents another certificate":          {state: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: []byte("another")}}}, own: own},
		"presents a certificate, none shared":   {state: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: own}}}},
		"an empty certificate matches nothing":  {state: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}},
		"no client certificate":                 {state: &tls.ConnectionState{}, own: own},
		"no TLS state":                          {state: nil, own: own},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := isTrustedPeer(tt.state, tt.own)

			assert.Equal(t, tt.want, got)
		})
	}
}

// Relays that share a certificate are peers: over a real handshake, a
// native-QUIC client that shows the relay's own certificate is trusted, and
// nothing else is. A client that offers h3 (a browser) is never asked for one.
func TestServer_TrustSharedCertificate(t *testing.T) {
	shared := newServerCert(t)
	another := newServerCert(t)
	native := []string{moqt.NextProtoMOQ}
	tests := map[string]struct {
		clientCert *tls.Certificate
		clientALPN []string
		wantAsked  bool // the server saw a client certificate
		wantPeer   bool
	}{
		"the shared certificate":              {clientCert: &shared, clientALPN: native, wantAsked: true, wantPeer: true},
		"another relay's certificate":         {clientCert: &another, clientALPN: native, wantAsked: true},
		"no certificate":                      {clientALPN: native},
		"a browser is not asked":              {clientCert: &shared, clientALPN: []string{"h3"}},
		"a client offering both is not asked": {clientCert: &shared, clientALPN: []string{"h3", moqt.NextProtoMOQ}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			srv := &Server{MOQServer: &moqt.Server{TLSConfig: &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{shared},
				NextProtos:   []string{"h3", moqt.NextProtoMOQ},
			}}}
			require.NoError(t, srv.TrustSharedCertificate())
			client := &tls.Config{
				MinVersion:         tls.VersionTLS13,
				NextProtos:         tt.clientALPN,
				InsecureSkipVerify: true, //nolint:gosec // the test's self-signed server certificate
			}
			if tt.clientCert != nil {
				client.Certificates = []tls.Certificate{*tt.clientCert}
			}

			state := handshake(t, client, srv.MOQServer.TLSConfig)

			assert.Equal(t, tt.wantAsked, len(state.PeerCertificates) > 0)
			assert.Equal(t, tt.wantPeer, isTrustedPeer(&state, srv.peerCertificate))
		})
	}
}

func TestServer_TrustSharedCertificate_NoCertificate(t *testing.T) {
	tests := map[string]*tls.Config{
		"no TLS config":   nil,
		"no certificates": {},
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			srv := &Server{MOQServer: &moqt.Server{TLSConfig: cfg}}

			err := srv.TrustSharedCertificate()

			assert.Error(t, err)
			assert.Nil(t, srv.peerCertificate)
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
