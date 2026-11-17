package geodns

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/reqsign"
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
	c := &feedClient{url: srv.URL + "/v1/relays/dns?zone=geo", token: "secret", signer: reqsigntest.Signer(t), client: reqsign.NoRedirectClient(feedTimeout)}

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
	c := &feedClient{url: srv.URL, token: "secret", signer: reqsigntest.Signer(t), client: reqsign.NoRedirectClient(feedTimeout)}

	_, err := c.fetch(t.Context())

	assert.ErrorContains(t, err, "302")
	assert.Zero(t, asked.Load())
}

// The peer identity settings, read by configFromEnv into the feed's signer.
func TestConfigFromEnv_PeerIdentity(t *testing.T) {
	certFile, keyFile := reqsigntest.WritePair(t, reqsigntest.Pair(t))
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	edDER, err := x509.MarshalPKCS8PrivateKey(ed)
	require.NoError(t, err)
	edKey := filepath.Join(t.TempDir(), "ed.key")
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
	certFile, keyFile := reqsigntest.WritePair(t, ed25519Pair(t))
	setRequired(t)
	t.Setenv("PEER_CERT_FILE", certFile)
	t.Setenv("PEER_KEY_FILE", keyFile)

	_, err := configFromEnv()

	assert.ErrorContains(t, err, "not ECDSA")
}

// ed25519Pair returns a certificate and its Ed25519 key: a pair that can't
// sign what the control plane verifies.
func ed25519Pair(tb testing.TB) tls.Certificate {
	tb.Helper()
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(tb, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "qumo-relay-tk1a-1"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, ed.Public(), ed)
	require.NoError(tb, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: ed}
}

// rewritePair writes pair's certificate to certFile and its key to keyFile,
// as PEM, in place of what they held.
func rewritePair(tb testing.TB, certFile, keyFile string, pair tls.Certificate) {
	tb.Helper()
	keyDER, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	require.NoError(tb, err)
	require.NoError(tb, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}), 0o600))
	require.NoError(tb, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))
}

// sentCertificate returns the certificate a feed request is signed with,
// as X-Relay-Certificate carries it.
func sentCertificate(tb testing.TB, signer *reqsign.Signer) string {
	tb.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/relays/dns", nil)
	require.NoError(tb, signer.Sign(req, nil))
	return req.Header.Get(reqsign.HeaderCertificate)
}

// certReloads returns qumo_dns_cert_reloads_total{cert="peer"} from reg, by
// result.
func certReloads(tb testing.TB, reg *prometheus.Registry) map[string]float64 {
	tb.Helper()
	families, err := reg.Gather()
	require.NoError(tb, err)
	got := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "qumo_dns_cert_reloads_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["cert"] == "peer" {
				got[labels["result"]] = m.GetCounter().GetValue()
			}
		}
	}
	return got
}

// A reload has the feed requests signed with a renewed peer certificate
// from the next request on; files it can't take leave the one in use.
func TestFeedCertReloader_reload(t *testing.T) {
	type want int
	const (
		kept want = iota
		renewed
	)
	tests := map[string]struct {
		// change rewrites the files and returns the pair they now hold.
		change      func(t *testing.T, certFile, keyFile string, old tls.Certificate) tls.Certificate
		want        want
		wantReloads map[string]float64 // qumo_dns_cert_reloads_total by result
	}{
		"files that didn't change": {
			change:      func(_ *testing.T, _, _ string, old tls.Certificate) tls.Certificate { return old },
			want:        kept,
			wantReloads: map[string]float64{},
		},
		"a renewed pair": {
			change: func(t *testing.T, certFile, keyFile string, _ tls.Certificate) tls.Certificate {
				pair := reqsigntest.Pair(t)
				rewritePair(t, certFile, keyFile, pair)
				return pair
			},
			want:        renewed,
			wantReloads: map[string]float64{"ok": 1},
		},
		"a certificate beside another's key": {
			change: func(t *testing.T, certFile, _ string, _ tls.Certificate) tls.Certificate {
				pair := reqsigntest.Pair(t)
				rewritePair(t, certFile, filepath.Join(t.TempDir(), "unused.key"), pair)
				return pair
			},
			want:        kept,
			wantReloads: map[string]float64{"error": 1},
		},
		"a key that can't sign": {
			change: func(t *testing.T, certFile, keyFile string, _ tls.Certificate) tls.Certificate {
				pair := ed25519Pair(t)
				rewritePair(t, certFile, keyFile, pair)
				return pair
			},
			want:        kept,
			wantReloads: map[string]float64{"error": 1},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			old := reqsigntest.Pair(t)
			certFile, keyFile := reqsigntest.WritePair(t, old)
			setRequired(t)
			t.Setenv("PEER_CERT_FILE", certFile)
			t.Setenv("PEER_KEY_FILE", keyFile)
			cfg, err := configFromEnv()
			require.NoError(t, err)
			reg := prometheus.NewRegistry()
			s, err := newService(cfg, reg)
			require.NoError(t, err)
			t.Cleanup(s.close)
			pair := tt.change(t, certFile, keyFile, old)

			s.feedCerts.reload()

			want := old
			if tt.want == renewed {
				want = pair
			}
			wantSigner, err := reqsign.New(want)
			require.NoError(t, err)
			assert.Equal(t, sentCertificate(t, wantSigner), sentCertificate(t, s.feed.signer), "the certificate feed requests are signed with")
			assert.Equal(t, tt.wantReloads, certReloads(t, reg), "qumo_dns_cert_reloads_total by result")
		})
	}
}

// Without a peer identity there is nothing to re-read.
func TestNewService_NoPeerIdentity(t *testing.T) {
	setRequired(t)
	cfg, err := configFromEnv()
	require.NoError(t, err)

	s, err := newService(cfg, prometheus.NewRegistry())
	require.NoError(t, err)
	t.Cleanup(s.close)

	assert.Nil(t, s.feedCerts)
}
