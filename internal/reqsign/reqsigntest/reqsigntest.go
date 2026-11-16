// Package reqsigntest gives tests a relay's peer identity and the control
// plane's check of what it signs (foalk-inc/qumo-deploy#1724), written here
// from the contract in internal/reqsign rather than shared with the
// signer, so that a client signing the wrong thing (another body, another
// URI) fails its tests instead of passing on headers that are merely set.
package reqsigntest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/reqsign"
)

// Pair returns a peer certificate and its ECDSA P-256 key, as the relay CA
// issues them (self-signed here: the chain is the control plane's check, not
// the signature's).
func Pair(tb testing.TB) tls.Certificate {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(tb, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "qumo-relay-tk1a-1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(tb, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// Signer returns a signer for a new Pair.
func Signer(tb testing.TB) *reqsign.Signer {
	tb.Helper()
	signer, err := reqsign.New(Pair(tb))
	require.NoError(tb, err)
	return signer
}

// Verify checks r, received with body, as the control plane does: the
// certificate from its header, and its key's signature over the context,
// method, request URI as received, timestamp and hex SHA-256 of body, one
// per line, with no newline after the last. It returns the timestamp
// signed. It reports by error rather than failing a test: a server handler
// calls it, off the test's goroutine, where require would stop only the
// handler and leave the test waiting; the test asserts on what it hands
// back.
func Verify(r *http.Request, body []byte) (int64, error) {
	certPEM, err := base64.StdEncoding.DecodeString(r.Header.Get(reqsign.HeaderCertificate))
	if err != nil {
		return 0, fmt.Errorf("certificate header: %w", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return 0, errors.New("certificate header: no PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return 0, fmt.Errorf("certificate: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(r.Header.Get(reqsign.HeaderSignature))
	if err != nil || len(sig) == 0 {
		return 0, errors.New("signature header: missing or not base64")
	}
	timestamp := r.Header.Get(reqsign.HeaderTimestamp)
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("timestamp header: %w", err)
	}
	sum := sha256.Sum256(body)
	msg := "qumo-relay-request\n" + r.Method + "\n" + r.URL.RequestURI() + "\n" + timestamp + "\n" + hex.EncodeToString(sum[:])
	if err := cert.CheckSignature(x509.ECDSAWithSHA256, []byte(msg), sig); err != nil {
		return 0, fmt.Errorf("signature: %w", err)
	}
	return ts, nil
}
