package relay

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// One TCP port serves plain HTTP and TLS, each to its own server.
func TestSplitTLS(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	tlsListener, plainListener := splitTLS(ln)
	addr := ln.Addr().String()

	answer := func(body string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, body) // not actionable: the client checks what it got
		})
	}
	// A test server only to borrow its certificate, and a client that
	// trusts it.
	certs := httptest.NewTLSServer(nil)
	defer certs.Close()
	plainServer := listenerServer{
		Server:   &http.Server{Handler: answer("plain"), ReadHeaderTimeout: time.Second},
		listener: plainListener,
	}
	tlsServer := listenerServer{
		Server: &http.Server{
			Handler:           answer("tls"),
			TLSConfig:         &tls.Config{Certificates: certs.TLS.Certificates, MinVersion: tls.VersionTLS12},
			ReadHeaderTimeout: time.Second,
		},
		listener: tlsListener,
		tls:      true,
	}
	served := make(chan error, 2)
	go func() { served <- plainServer.ListenAndServe() }()
	go func() { served <- tlsServer.ListenAndServe() }()

	client := certs.Client()
	client.Timeout = 5 * time.Second
	get := func(url string) string {
		rsp, err := client.Get(url)
		require.NoError(t, err)
		defer func() { _ = rsp.Body.Close() }() // not actionable: the test is over
		body, err := io.ReadAll(rsp.Body)
		require.NoError(t, err)
		return string(body)
	}

	assert.Equal(t, "plain", get("http://"+addr+"/health"))
	assert.Equal(t, "tls", get("https://"+addr+"/"))
	assert.Equal(t, "plain", get("http://"+addr+"/metrics"), "plain HTTP still works after a TLS connection")
	assert.Equal(t, addr, tlsListener.Addr().String())
	assert.Equal(t, addr, plainListener.Addr().String())

	// Shutting one server down closes the port, which ends the other as a
	// shutdown too.
	require.NoError(t, plainServer.Close())
	for range 2 {
		select {
		case err := <-served:
			assert.ErrorIs(t, err, http.ErrServerClosed)
		case <-time.After(5 * time.Second):
			require.FailNow(t, "a server kept serving a closed port")
		}
	}
	_, err = net.DialTimeout("tcp", addr, time.Second)
	assert.Error(t, err, "the port is closed")
}

// Accept on either side ends once the port is closed.
func TestSplitListener_Accept_Closed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	tlsListener, plainListener := splitTLS(ln)

	require.NoError(t, tlsListener.Close())

	_, err = tlsListener.Accept()
	assert.ErrorIs(t, err, net.ErrClosed)
	_, err = plainListener.Accept()
	assert.ErrorIs(t, err, net.ErrClosed)
	assert.NoError(t, plainListener.Close(), "closing again is fine")
}

func TestPrefixedConn_Read(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }() // not actionable: the test is over
	go func() {
		_, _ = server.Write([]byte("ET /")) // not actionable: the reader checks what it got
		_ = server.Close()                  // not actionable: the test is over
	}()
	conn := &prefixedConn{Conn: client, prefix: []byte("G")}

	got, err := io.ReadAll(conn)

	require.NoError(t, err)
	assert.Equal(t, "GET /", string(got))
}
