// Package relaysign signs a relay's requests to its control plane (the key
// set, usage reports, the DNS feed) with the relay's peer identity, so that
// the control plane knows which relay is calling from a key only that relay
// holds, not from a bearer token every relay of the deployment shares.
//
// What is signed, and how it is sent, is the control plane's contract
// (qumo-dev/qumo#601): the certificate as base64 of its PEM in
// X-Relay-Certificate, the time in unix seconds in X-Relay-Timestamp, and in
// X-Relay-Signature the ECDSA signature (ASN.1, base64) over SHA-256 of
//
//	qumo-relay-request
//	<method>
//	<request URI: path and query, as sent>
//	<timestamp>
//	<hex SHA-256 of the body>
//
// with no newline after the last line. The control plane takes a timestamp
// within two minutes of its clock and keeps no record of what it took: every
// request signed here is a read or a resend that adds nothing.
//
// The host is not signed. Each deployment has its own relay CA, so a
// signature made for one control plane verifies at no other; a proxy in
// front of the control plane may rewrite Host, which would break a check
// that covered it; and a client that sends signed requests must follow no
// redirect, so that no other host is handed one (the clients here don't).
package relaysign

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// The headers a signed request carries.
const (
	HeaderCertificate = "X-Relay-Certificate"
	HeaderTimestamp   = "X-Relay-Timestamp"
	HeaderSignature   = "X-Relay-Signature"
)

// requestContext opens what is signed, so that a signature made for a
// request stands for nothing else the key signs (a heartbeat opens with its
// own).
const requestContext = "qumo-relay-request"

// Signer signs requests with one relay's peer certificate and key. A nil
// Signer signs nothing, so a client without a peer identity calls Sign all
// the same.
type Signer struct {
	certificate string // base64 of the certificate's PEM, as sent
	key         crypto.Signer
	now         func() time.Time
}

// Load reads the peer certificate and key (PEER_CERT_FILE, PEER_KEY_FILE).
// The key must be ECDSA, which is what the control plane verifies. The files
// are read once: a certificate renewed on disk is used after a restart.
func Load(certFile, keyFile string) (*Signer, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load the peer certificate: %w", err)
	}
	return New(pair)
}

// New returns a Signer for pair, whose first certificate is the relay's own.
func New(pair tls.Certificate) (*Signer, error) {
	if len(pair.Certificate) == 0 {
		return nil, errors.New("no certificate")
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("the peer key is %T, not ECDSA: the control plane verifies ECDSA signatures only", pair.PrivateKey)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]})
	return &Signer{
		certificate: base64.StdEncoding.EncodeToString(certPEM),
		key:         key,
		now:         time.Now,
	}, nil
}

// Sign adds the signature headers to req, whose body is body (nil for
// none), signed now: a request sent again is signed again, so a retry is
// never refused for its age.
func (s *Signer) Sign(req *http.Request, body []byte) error {
	if s == nil {
		return nil
	}
	timestamp := strconv.FormatInt(s.now().Unix(), 10)
	digest := sha256.Sum256(signed(req.Method, req.URL.RequestURI(), timestamp, body))
	sig, err := s.key.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		return fmt.Errorf("sign the request: %w", err)
	}
	req.Header.Set(HeaderCertificate, s.certificate)
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderSignature, base64.StdEncoding.EncodeToString(sig))
	return nil
}

// signed is the string a request's signature is over.
func signed(method, requestURI, timestamp string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(requestContext + "\n" + method + "\n" + requestURI + "\n" + timestamp + "\n" + hex.EncodeToString(sum[:]))
}

// LogUnauthorized logs a 401 from the control plane for target, with the
// server's clock beside this one: a signed request is refused when the two
// are more than two minutes apart, and nothing else in a 401 says so.
func LogUnauthorized(target string, resp *http.Response) {
	if resp.StatusCode != http.StatusUnauthorized {
		return
	}
	slog.Warn("control plane refused the relay's request (401): check its credential and, for a signed request, the clock",
		"target", target, "server_date", resp.Header.Get("Date"), "local_time", time.Now().UTC().Format(http.TimeFormat))
}
