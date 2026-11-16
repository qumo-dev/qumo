package geodns

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/reqsign/reqsigntest"
)

// feedRequest is what the test feed saw of a request: its bearer, and
// whether its signature verified as the control plane checks it.
type feedRequest struct {
	bearer string
	err    error
}

// With the node relay's peer identity, each feed read carries a signature
// the control plane accepts, and the token still.
func TestFeedClient_Fetch_Signed(t *testing.T) {
	seen := make(chan feedRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := reqsigntest.Verify(r, nil)
		seen <- feedRequest{bearer: r.Header.Get("Authorization"), err: err}
		if _, err := w.Write([]byte(`{}`)); err != nil {
			t.Errorf("write the feed: %v", err)
		}
	}))
	defer srv.Close()
	c := &feedClient{url: srv.URL + "/v1/relays/dns?zone=geo", token: "secret", signer: reqsigntest.Signer(t), client: newFeedHTTPClient()}

	_, err := c.fetch(t.Context())

	require.NoError(t, err)
	got := <-seen
	assert.NoError(t, got.err)
	assert.Equal(t, "Bearer secret", got.bearer)
}

// A feed that answers with a redirect is an error, and the place it names
// is never asked: it would get the signature, which the control plane takes
// for two minutes.
func TestFeedClient_Fetch_FollowsNoRedirect(t *testing.T) {
	var asked atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		asked.Add(1)
	}))
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/capture", http.StatusFound)
	}))
	defer srv.Close()
	c := &feedClient{url: srv.URL, token: "secret", signer: reqsigntest.Signer(t), client: newFeedHTTPClient()}

	_, err := c.fetch(t.Context())

	assert.ErrorContains(t, err, "302")
	assert.Zero(t, asked.Load())
}

// The peer identity settings, read by configFromEnv into the feed's signer.
func TestConfigFromEnv_PeerIdentity(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writePeerPair(t, dir)
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	edDER, err := x509.MarshalPKCS8PrivateKey(ed)
	require.NoError(t, err)
	edKey := filepath.Join(dir, "ed.key")
	require.NoError(t, os.WriteFile(edKey, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: edDER}), 0o600))

	tests := map[string]struct {
		cert, key  string
		wantSigner bool
		wantErr    string
	}{
		"neither: unsigned":           {},
		"both: signed":                {cert: certFile, key: keyFile, wantSigner: true},
		"a key without a certificate": {key: keyFile, wantErr: "must be set together"},
		"a certificate without a key": {cert: certFile, wantErr: "must be set together"},
		"a key that does not match":   {cert: certFile, key: edKey, wantErr: "PEER_CERT_FILE"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			setRequired(t)
			t.Setenv("PEER_CERT_FILE", tt.cert)
			t.Setenv("PEER_KEY_FILE", tt.key)

			cfg, err := configFromEnv()

			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantSigner, cfg.feedSigner != nil)
		})
	}
}

// A key that is not ECDSA cannot sign what the control plane verifies:
// startup says so rather than read the feed unsigned.
func TestConfigFromEnv_PeerIdentityNotECDSA(t *testing.T) {
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "qumo-relay-tk1a-1"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, ed.Public(), ed)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(ed)
	require.NoError(t, err)
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "ed.crt"), filepath.Join(dir, "ed.key")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))
	setRequired(t)
	t.Setenv("PEER_CERT_FILE", certFile)
	t.Setenv("PEER_KEY_FILE", keyFile)

	_, err = configFromEnv()

	assert.ErrorContains(t, err, "not ECDSA")
}

// writePeerPair writes a relay's peer certificate and key into dir.
func writePeerPair(tb testing.TB, dir string) (certFile, keyFile string) {
	tb.Helper()
	pair := reqsigntest.Pair(tb)
	der, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	require.NoError(tb, err)
	certFile, keyFile = filepath.Join(dir, "peer.crt"), filepath.Join(dir, "peer.key")
	require.NoError(tb, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}), 0o600))
	require.NoError(tb, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600))
	return certFile, keyFile
}
