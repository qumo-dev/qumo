package hls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qumo-dev/qumo/internal/devcert"
	"github.com/qumo-dev/qumo/internal/relay"
	"github.com/qumo-dev/qumo/token"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// relayTLSConfig is the egress's verification policy turned into a tls.Config:
// verify against the system roots by default, trust a named relay cert when one
// is given, or skip verification entirely when insecure is set. insecure
// dominates a caFile, matching crypto/tls.
func TestRelayTLSConfig(t *testing.T) {
	cert := writeCertFile(t, "relay.pem")
	bogus := filepath.Join(t.TempDir(), "absent.pem") // never written, so missing

	t.Run("system roots by default", func(t *testing.T) {
		tc, err := relayTLSConfig("", false)
		require.NoError(t, err)
		assert.Equal(t, uint16(tls.VersionTLS13), tc.MinVersion)
		assert.False(t, tc.InsecureSkipVerify, "verification is on by default")
		assert.Nil(t, tc.RootCAs, "nil RootCAs means crypto/tls uses the system root store")
	})

	t.Run("trusts a named relay cert", func(t *testing.T) {
		tc, err := relayTLSConfig(cert, false)
		require.NoError(t, err)
		assert.False(t, tc.InsecureSkipVerify)
		require.NotNil(t, tc.RootCAs, "a named cert overrides the system roots with its own pool")
	})

	t.Run("insecure skips verification", func(t *testing.T) {
		tc, err := relayTLSConfig("", true)
		require.NoError(t, err)
		assert.True(t, tc.InsecureSkipVerify)
		assert.Nil(t, tc.RootCAs)
	})

	t.Run("insecure dominates a bad caFile", func(t *testing.T) {
		// The bogus path is never read: insecure short-circuits before loading.
		tc, err := relayTLSConfig(bogus, true)
		require.NoError(t, err)
		assert.True(t, tc.InsecureSkipVerify)
	})

	t.Run("missing caFile is an error", func(t *testing.T) {
		_, err := relayTLSConfig(bogus, false)
		assert.Error(t, err)
	})

	t.Run("caFile without a certificate is an error", func(t *testing.T) {
		empty := filepath.Join(t.TempDir(), "empty.pem")
		require.NoError(t, os.WriteFile(empty, []byte("not a certificate"), 0o600))
		_, err := relayTLSConfig(empty, false)
		assert.Error(t, err)
	})
}

// writeCertFile writes a self-signed PEM cert to a temp file and returns its
// path. Mirrors the relay cert a dev relay (or `mage cert`) would present.
func writeCertFile(tb testing.TB, name string) string {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), name)
	require.NoError(tb, os.WriteFile(path, selfSignedCertPEM(tb), 0o600))
	return path
}

// selfSignedCertPEM generates a throwaway self-signed certificate as PEM — the
// same shape internal/loadgen uses for its --ca fixture.
func selfSignedCertPEM(tb testing.TB) []byte {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(tb, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(tb, err)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(tb, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestFeedConfig_Validate(t *testing.T) {
	ca, err := devcert.NewCA("relay-ca", time.Hour)
	require.NoError(t, err)
	peerCert, peerKey := writeLeaf(t, ca, devcert.Leaf{CommonName: "egress-1", DNSNames: []string{relay.PeeringName}, Validity: time.Hour})
	unnamedCert, unnamedKey := writeLeaf(t, ca, devcert.Leaf{CommonName: "egress-2", Validity: time.Hour})
	key, err := token.GenerateKey("hls")
	require.NoError(t, err)
	exact, err := token.GenerateKey("hls/live")
	require.NoError(t, err)

	tests := map[string]struct {
		cfg     feedConfig
		wantErr string
	}{
		"no client certificate":     {cfg: feedConfig{relayURL: "https://relay:4433"}},
		"a credential in the query": {cfg: feedConfig{relayURL: "https://relay:4433/live?jwt=h.p.s"}},
		"a malformed URL":           {cfg: feedConfig{relayURL: "moqt://[bad/live"}, wantErr: "invalid URL"},
		"a peer certificate over native QUIC": {
			cfg: feedConfig{relayURL: "moqt://relay:4433", certFile: peerCert, keyFile: peerKey},
		},
		"a peer certificate over WebTransport": {
			cfg:     feedConfig{relayURL: "https://relay:4433", certFile: peerCert, keyFile: peerKey},
			wantErr: "moqt://",
		},
		"cert without key": {cfg: feedConfig{relayURL: "moqt://relay:4433", certFile: peerCert}, wantErr: "together"},
		"key without cert": {cfg: feedConfig{relayURL: "moqt://relay:4433", keyFile: peerKey}, wantErr: "together"},
		"a certificate without the peering name": {
			cfg:     feedConfig{relayURL: "moqt://relay:4433", certFile: unnamedCert, keyFile: unnamedKey},
			wantErr: "peering name",
		},
		"a certificate that doesn't load": {
			cfg:     feedConfig{relayURL: "moqt://relay:4433", certFile: "absent.pem", keyFile: "absent.key"},
			wantErr: "RELAY_CERT_FILE",
		},
		"a peer certificate beside a credential": {
			cfg:     feedConfig{relayURL: "moqt://relay:4433/?jwt=h.p.s", certFile: peerCert, keyFile: peerKey},
			wantErr: "never reads",
		},
		"a peer certificate beside a signing key": {
			cfg:     feedConfig{relayURL: "moqt://relay:4433", certFile: peerCert, keyFile: peerKey, signingKey: &key},
			wantErr: "set one",
		},
		"a signing key that covers the path": {
			cfg: feedConfig{relayURL: "https://relay:4433", trackPath: "/hls/live", signingKey: &key},
		},
		"a signing key confined elsewhere": {
			cfg:     feedConfig{relayURL: "https://relay:4433", trackPath: "/room/1", signingKey: &key},
			wantErr: "RELAY_TRACK_PATH",
		},
		// The grant's root is the path's directory: a key confined to the
		// path itself can't grant it.
		"a signing key confined to the path itself": {
			cfg:     feedConfig{relayURL: "https://relay:4433", trackPath: "/hls/live", signingKey: &exact},
			wantErr: "must lie beneath the key's prefix",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := tt.cfg.validate()

			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			assert.NoError(t, err)
		})
	}
}

// TestFeedConfig_Validate_MalformedURLHidesCredential verifies a RELAY_URL
// that doesn't parse is reported without quoting it: url.Parse's error would
// quote its whole input, credential included.
func TestFeedConfig_Validate_MalformedURLHidesCredential(t *testing.T) {
	cfg := feedConfig{relayURL: "moqt://[bad/live?jwt=h.p.s"}

	err := cfg.validate()

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "jwt=h.p.s")
}

// writeLeaf issues leaf from ca and writes it and its key as PEM files.
func writeLeaf(tb testing.TB, ca *devcert.CA, leaf devcert.Leaf) (certFile, keyFile string) {
	tb.Helper()
	cert, err := ca.Issue(leaf)
	require.NoError(tb, err)
	certPEM, keyPEM, err := devcert.EncodePEM(cert)
	require.NoError(tb, err)
	dir := tb.TempDir()
	certFile, keyFile = filepath.Join(dir, "leaf.crt"), filepath.Join(dir, "leaf.key")
	require.NoError(tb, os.WriteFile(certFile, certPEM, 0o600))
	require.NoError(tb, os.WriteFile(keyFile, keyPEM, 0o600))
	return certFile, keyFile
}

func TestFeedConfig_Target(t *testing.T) {
	key, err := token.GenerateKey("hls")
	require.NoError(t, err)

	t.Run("without a signing key the URL is dialed as it is", func(t *testing.T) {
		cfg := feedConfig{relayURL: "https://relay:4433/?jwt=h.p.s", trackPath: "/hls/live"}

		got, err := cfg.target()

		require.NoError(t, err)
		assert.Equal(t, cfg.relayURL, got)
	})

	t.Run("a signing key replaces the credential with one for the path alone", func(t *testing.T) {
		cfg := feedConfig{relayURL: "https://relay:4433/?jwt=h.p.s&x=1", trackPath: "/hls/live", signingKey: &key}

		got, err := cfg.target()

		require.NoError(t, err)
		u, err := url.Parse(got)
		require.NoError(t, err)
		assert.Equal(t, "1", u.Query().Get("x"), "the rest of the query is kept")
		claims, err := token.Verify(u.Query().Get("jwt"), map[string]token.Key{key.ID: key.Public()}, time.Now())
		require.NoError(t, err)
		g := claims.Grant
		assert.Equal(t, token.Grant{Root: "hls", Subscribe: []string{"/live"}}, g)
		assert.True(t, g.Allows(token.ActionSubscribe, "hls/live"))
		assert.False(t, g.Allows(token.ActionSubscribe, "hls/other"), "no other broadcast")
		assert.False(t, g.Allows(token.ActionSubscribe, "hls/live/deeper"), "nothing beneath it")
		assert.False(t, g.Allows(token.ActionPublish, "hls/live"), "it publishes nothing")
	})

	t.Run("each connect signs a new one", func(t *testing.T) {
		cfg := feedConfig{relayURL: "https://relay:4433", trackPath: "/hls/live", signingKey: &key}

		first, err := cfg.target()
		require.NoError(t, err)
		second, err := cfg.target()
		require.NoError(t, err)

		assert.NotEqual(t, first, second)
	})
}

func TestSubscribeGrant(t *testing.T) {
	tests := map[string]struct {
		path string
		want token.Grant
	}{
		"beneath a directory": {path: "/acme/room/42/7", want: token.Grant{Root: "acme/room/42", Subscribe: []string{"/7"}}},
		"one segment":         {path: "/live", want: token.Grant{Subscribe: []string{"/live"}}},
		"no leading slash":    {path: "hls/live", want: token.Grant{Root: "hls", Subscribe: []string{"/live"}}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, subscribeGrant(tt.path))
		})
	}
}
