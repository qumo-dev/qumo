package relay

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/devcert"
	"github.com/qumo-dev/qumo/internal/reqsign"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reloadFixture is a relay's certificate files in a test directory, the
// working directory (CA_FILE is relative), and a reloader that started with
// them.
type reloadFixture struct {
	r        *certReloader
	ca       *devcert.CA // the relay CA the relay started with
	publicCA *devcert.CA // what issues public certificates
	public   tls.Certificate
	peer     tls.Certificate
}

// publicLeaf is a relay's public certificate.
func publicLeaf() devcert.Leaf {
	return devcert.Leaf{CommonName: "localhost", DNSNames: []string{"localhost"}, ExtKeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
}

// newReloadFixture writes public.crt/.key, ca.crt and peer.crt/.key to a new
// directory, makes it the working directory, and starts a reloader on them.
func newReloadFixture(tb testing.TB) *reloadFixture {
	tb.Helper()
	f := &reloadFixture{ca: testCA(tb), publicCA: testCA(tb)}
	f.public = issue(tb, f.publicCA, publicLeaf())
	f.peer = issue(tb, f.ca, peerLeaf("relay-1"))
	dir := tb.TempDir()
	writePair(tb, dir, "public", f.public)
	writePair(tb, dir, "peer", f.peer)
	require.NoError(tb, os.WriteFile(dir+"/ca.crt", f.ca.CertPEM(), 0o600))
	tb.Chdir(dir)
	trust, err := loadPeerTrust("ca.crt", "peer.crt", "peer.key", true)
	require.NoError(tb, err)
	signer, err := reqsign.New(f.peer)
	require.NoError(tb, err)
	public, err := loadPublicCertificate("public.crt", "public.key")
	require.NoError(tb, err)
	f.r = newCertReloader(certFiles{cert: "public.crt", key: "public.key", ca: "ca.crt", peerCert: "peer.crt", peerKey: "peer.key"}, public, trust, signer)
	return f
}

// write replaces the file name in the working directory.
func write(tb testing.TB, name string, data []byte) {
	tb.Helper()
	require.NoError(tb, os.WriteFile(name, data, 0o600))
}

// sentCertificate returns the certificate header signer sends.
func sentCertificate(tb testing.TB, signer *reqsign.Signer) string {
	tb.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/relays/trust", nil)
	require.NoError(tb, signer.Sign(req, nil))
	return req.Header.Get(reqsign.HeaderCertificate)
}

// What a reload takes: renewed certificates that check out, whole. What it
// refuses leaves the certificates in use as they were.
func TestCertReloader_reload(t *testing.T) {
	newCA := testCA(t)
	type want int
	const (
		kept want = iota
		renewed
	)
	tests := map[string]struct {
		// change rewrites the files and returns the certificates they now
		// hold, nil for those it leaves.
		change     func(t *testing.T, f *reloadFixture) (public, peer *tls.Certificate)
		wantPublic want
		wantPeer   want
		wantNewCA  bool // the CA in use trusts newCA too
	}{
		"files that didn't change": {
			change: func(*testing.T, *reloadFixture) (*tls.Certificate, *tls.Certificate) { return nil, nil },
		},
		"a renewed public certificate": {
			change: func(t *testing.T, f *reloadFixture) (*tls.Certificate, *tls.Certificate) {
				c := issue(t, f.publicCA, publicLeaf())
				writePair(t, ".", "public", c)
				return &c, nil
			},
			wantPublic: renewed,
		},
		"a public certificate beside another's key": {
			change: func(t *testing.T, f *reloadFixture) (*tls.Certificate, *tls.Certificate) {
				c := issue(t, f.publicCA, publicLeaf())
				certPEM, _, err := devcert.EncodePEM(c)
				require.NoError(t, err)
				write(t, "public.crt", certPEM)
				return &c, nil
			},
		},
		"a public key file that can't be read": {
			change: func(t *testing.T, f *reloadFixture) (*tls.Certificate, *tls.Certificate) {
				c := issue(t, f.publicCA, publicLeaf())
				writePair(t, ".", "public", c)
				require.NoError(t, os.Remove("public.key"))
				return &c, nil
			},
		},
		"a renewed peer certificate": {
			change: func(t *testing.T, f *reloadFixture) (*tls.Certificate, *tls.Certificate) {
				c := issue(t, f.ca, peerLeaf("relay-1"))
				writePair(t, ".", "peer", c)
				return nil, &c
			},
			wantPeer: renewed,
		},
		"a peer certificate from another CA": {
			change: func(t *testing.T, f *reloadFixture) (*tls.Certificate, *tls.Certificate) {
				c := issue(t, newCA, peerLeaf("relay-1"))
				writePair(t, ".", "peer", c)
				return nil, &c
			},
		},
		"a peer certificate without the peering name": {
			change: func(t *testing.T, f *reloadFixture) (*tls.Certificate, *tls.Certificate) {
				c := issue(t, f.ca, devcert.Leaf{CommonName: "relay-1"})
				writePair(t, ".", "peer", c)
				return nil, &c
			},
		},
		// A rotation to a new CA: the bundle holds both while relays move
		// to certificates from the new one.
		"a CA bundle with a new CA, and a peer certificate from it": {
			change: func(t *testing.T, f *reloadFixture) (*tls.Certificate, *tls.Certificate) {
				write(t, "ca.crt", slices.Concat(f.ca.CertPEM(), newCA.CertPEM()))
				c := issue(t, newCA, peerLeaf("relay-1"))
				writePair(t, ".", "peer", c)
				return nil, &c
			},
			wantPeer:  renewed,
			wantNewCA: true,
		},
		"a CA bundle with a new CA alone": {
			change: func(t *testing.T, f *reloadFixture) (*tls.Certificate, *tls.Certificate) {
				write(t, "ca.crt", slices.Concat(f.ca.CertPEM(), newCA.CertPEM()))
				return nil, nil
			},
			wantNewCA: true,
		},
		"a CA file that no longer holds the peer certificate's CA": {
			change: func(t *testing.T, f *reloadFixture) (*tls.Certificate, *tls.Certificate) {
				write(t, "ca.crt", newCA.CertPEM())
				return nil, nil
			},
		},
		"a CA file with no certificate": {
			change: func(t *testing.T, f *reloadFixture) (*tls.Certificate, *tls.Certificate) {
				write(t, "ca.crt", []byte("not a certificate"))
				return nil, nil
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := newReloadFixture(t)
			public, peer := tt.change(t, f)

			f.r.reload()

			wantPublic, wantPeer := &f.public, &f.peer
			if tt.wantPublic == renewed {
				wantPublic = public
			}
			if tt.wantPeer == renewed {
				wantPeer = peer
			}
			got, err := f.r.certificate(nil)
			require.NoError(t, err)
			assert.Equal(t, wantPublic.Certificate, got.Certificate, "the public certificate in use")
			assert.Equal(t, wantPeer.Certificate[0], f.r.ownCertificate(), "the peer certificate in use")
			want, err := reqsign.New(*wantPeer)
			require.NoError(t, err)
			assert.Equal(t, sentCertificate(t, want), sentCertificate(t, f.r.signer), "the certificate requests are signed with")
			wantCA := rootsOf(f.ca)
			if tt.wantNewCA {
				wantCA.AddCert(newCA.Cert)
			}
			assert.True(t, wantCA.Equal(f.r.currentTrust().ca), "the CA in use")
		})
	}
}

// echoServer serves TLS on a local port with cfg, echoing what each
// connection sends, until the test ends.
func echoServer(tb testing.TB, cfg *tls.Config) string {
	tb.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	require.NoError(tb, err)
	tb.Cleanup(func() { assert.NoError(tb, ln.Close()) })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // the listener is closed
			}
			go func() {
				defer conn.Close()
				if _, err := io.Copy(conn, conn); err != nil {
					return // the connection failed: the test's side reports it
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// dialEcho opens a TLS connection to addr with cfg and returns it with the
// certificate the server presented.
func dialEcho(tb testing.TB, addr string, cfg *tls.Config) (*tls.Conn, error) {
	tb.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, cfg)
	if err != nil {
		return nil, err
	}
	tb.Cleanup(func() { assert.NoError(tb, conn.Close()) })
	require.NoError(tb, conn.SetDeadline(time.Now().Add(5*time.Second)))
	// A round trip, so a server that refuses the client's certificate has
	// said so.
	if err := echo(conn); err != nil {
		return nil, err
	}
	return conn, nil
}

// echo sends a byte on conn and reads it back.
func echo(conn *tls.Conn) error {
	if _, err := conn.Write([]byte{42}); err != nil {
		return err
	}
	_, err := io.ReadFull(conn, make([]byte, 1))
	return err
}

// A reload changes what new handshakes are given, and leaves connections
// already established alone: a browser's, a peer's that dialed this relay.
// After a rotation to a new CA, this relay's own dialer verifies a peer
// from that CA, and refused it before.
func TestCertReloader_handshakes(t *testing.T) {
	f := newReloadFixture(t)
	newCA := testCA(t)
	addr := echoServer(t, serverTLS(f.r.publicTLS(), f.r.currentTrust))
	browser := &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{"h3"}, ServerName: "localhost", RootCAs: rootsOf(f.publicCA)}
	// Another relay, trusting both CAs, with a certificate from the old.
	other := issue(t, f.ca, peerLeaf("relay-2"))
	both := slices.Concat(f.ca.CertPEM(), newCA.CertPEM())
	otherTrust, err := NewPeerTrust(both, &other)
	require.NoError(t, err)
	// A peer that already holds a certificate from the new CA.
	newPeer := issue(t, newCA, peerLeaf("relay-3"))
	newPeerTrust, err := NewPeerTrust(newCA.CertPEM(), &newPeer)
	require.NoError(t, err)
	newPeerAddr := echoServer(t, newPeerTrust.ServerTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{f.public}, NextProtos: []string{moqt.NextProtoMOQ}}))
	ownDialer := dialerTLS(f.r.currentTrust)

	browserConn, err := dialEcho(t, addr, browser)
	require.NoError(t, err)
	peerConn, err := dialEcho(t, addr, otherTrust.DialerTLS())
	require.NoError(t, err)
	_, err = dialEcho(t, newPeerAddr, ownDialer)
	require.ErrorContains(t, err, "certificate signed by unknown authority", "before the rotation, a peer from the new CA isn't trusted")

	public := issue(t, f.publicCA, publicLeaf())
	writePair(t, ".", "public", public)
	write(t, "ca.crt", both)
	peer := issue(t, newCA, peerLeaf("relay-1"))
	writePair(t, ".", "peer", peer)
	f.r.reload()

	assert.NoError(t, echo(browserConn), "the browser's connection carries on")
	assert.Equal(t, f.public.Certificate[0], browserConn.ConnectionState().PeerCertificates[0].Raw, "with the certificate it was given")
	assert.NoError(t, echo(peerConn), "the peer's connection carries on")
	assert.Equal(t, f.peer.Certificate[0], peerConn.ConnectionState().PeerCertificates[0].Raw, "with the certificate it was given")

	browserConn, err = dialEcho(t, addr, browser)
	require.NoError(t, err)
	assert.Equal(t, public.Certificate[0], browserConn.ConnectionState().PeerCertificates[0].Raw, "a new browser is given the renewed public certificate")
	peerConn, err = dialEcho(t, addr, otherTrust.DialerTLS())
	require.NoError(t, err)
	assert.Equal(t, peer.Certificate[0], peerConn.ConnectionState().PeerCertificates[0].Raw, "a new peer is given the renewed peer certificate")
	newPeerConn, err := dialEcho(t, newPeerAddr, ownDialer)
	require.NoError(t, err, "after it, this relay dials a peer from the new CA")
	assert.Equal(t, newPeer.Certificate[0], newPeerConn.ConnectionState().PeerCertificates[0].Raw)
}

// run reloads once a certReloadInterval, and stops with its context.
func TestCertReloader_run(t *testing.T) {
	f := newReloadFixture(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.r.run(ctx)
		}()
		public := issue(t, f.publicCA, publicLeaf())
		writePair(t, ".", "public", public)

		time.Sleep(certReloadInterval - time.Second)
		synctest.Wait()
		got, err := f.r.certificate(nil)
		require.NoError(t, err)
		assert.Equal(t, f.public.Certificate[0], got.Certificate[0], "not read before the interval")

		time.Sleep(time.Second)
		synctest.Wait()
		got, err = f.r.certificate(nil)
		require.NoError(t, err)
		assert.Equal(t, public.Certificate[0], got.Certificate[0], "read once the interval is up")

		cancel()
		<-done
	})
}

// A reloader for a relay with no peer identity, or no CA, reads the files it
// has.
func TestCertReloader_reload_PeerSettings(t *testing.T) {
	ca := testCA(t)
	newCA := testCA(t)
	publicCA := testCA(t)
	tests := map[string]struct {
		files     certFiles
		caPEM     []byte
		wantNewCA bool
	}{
		"the CA alone": {files: certFiles{cert: "public.crt", key: "public.key", ca: "ca.crt"}, caPEM: ca.CertPEM(), wantNewCA: true},
		"no CA":        {files: certFiles{cert: "public.crt", key: "public.key"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writePair(t, dir, "public", issue(t, publicCA, publicLeaf()))
			require.NoError(t, os.WriteFile(dir+"/ca.crt", ca.CertPEM(), 0o600))
			t.Chdir(dir)
			trust, err := NewPeerTrust(tt.caPEM, nil)
			require.NoError(t, err)
			public, err := loadPublicCertificate("public.crt", "public.key")
			require.NoError(t, err)
			r := newCertReloader(tt.files, public, trust, nil)
			renewed := issue(t, publicCA, publicLeaf())
			writePair(t, ".", "public", renewed)
			write(t, "ca.crt", slices.Concat(ca.CertPEM(), newCA.CertPEM()))

			r.reload()

			got, err := r.certificate(nil)
			require.NoError(t, err)
			assert.Equal(t, renewed.Certificate[0], got.Certificate[0])
			assert.Nil(t, r.ownCertificate())
			both := rootsOf(ca)
			both.AddCert(newCA.Cert)
			assert.Equal(t, tt.wantNewCA, r.currentTrust().HasCA() && r.currentTrust().ca.Equal(both), "the CA in use trusts the new one")
		})
	}
}
