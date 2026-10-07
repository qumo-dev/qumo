package relay

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"

	"github.com/qumo-dev/qumo/internal/devcert"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writePair writes cert and its key as name.crt and name.key in dir.
func writePair(tb testing.TB, dir, name string, cert tls.Certificate) {
	tb.Helper()
	certPEM, keyPEM, err := devcert.EncodePEM(cert)
	require.NoError(tb, err)
	require.NoError(tb, os.WriteFile(filepath.Join(dir, name+".crt"), certPEM, 0o600))
	require.NoError(tb, os.WriteFile(filepath.Join(dir, name+".key"), keyPEM, 0o600))
}

// The settings: which combinations of CA_FILE, PEER_CERT_FILE, PEER_KEY_FILE
// and PEERS the command accepts, and that each refusal names the setting.
// What makes a certificate a valid peer certificate is TestNewPeerTrust's.
func Test_loadPeerTrust(t *testing.T) {
	ca := testCA(t)
	otherCA := testCA(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ca.crt"), ca.CertPEM(), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "empty.crt"), nil, 0o600))
	writePair(t, dir, "peer", issue(t, ca, peerLeaf("relay-1")))
	writePair(t, dir, "foreign", issue(t, otherCA, peerLeaf("relay-x")))
	// CA_FILE must be relative: the test runs from the directory.
	t.Chdir(dir)

	tests := map[string]struct {
		ca, cert, key string
		peers         bool
		wantCA        bool
		wantIdentity  string
		wantErrText   string
	}{
		"standalone":                          {},
		"CA only":                             {ca: "ca.crt", wantCA: true},
		"CA and a peer identity":              {ca: "ca.crt", cert: "peer.crt", key: "peer.key", wantCA: true, wantIdentity: "relay-1"},
		"full peering":                        {ca: "ca.crt", cert: "peer.crt", key: "peer.key", peers: true, wantCA: true, wantIdentity: "relay-1"},
		"the certificate alone":               {ca: "ca.crt", cert: "peer.crt", wantErrText: "PEER_CERT_FILE and PEER_KEY_FILE must be set together"},
		"the key alone":                       {ca: "ca.crt", key: "peer.key", wantErrText: "PEER_CERT_FILE and PEER_KEY_FILE must be set together"},
		"a peer identity without a CA":        {cert: "peer.crt", key: "peer.key", wantErrText: "PEER_CERT_FILE needs CA_FILE"},
		"PEERS without anything":              {peers: true, wantErrText: "PEERS needs CA_FILE, PEER_CERT_FILE and PEER_KEY_FILE"},
		"PEERS with the CA only":              {ca: "ca.crt", peers: true, wantErrText: "PEERS needs CA_FILE, PEER_CERT_FILE and PEER_KEY_FILE"},
		"a CA file that isn't there":          {ca: "nope.crt", wantErrText: "failed to load CA_FILE"},
		"a CA file with no certificate":       {ca: "empty.crt", wantErrText: "failed to load CA_FILE"},
		"an absolute CA path":                 {ca: filepath.Join(dir, "ca.crt"), wantErrText: "CA_FILE must be a relative path"},
		"a CA path that climbs out":           {ca: filepath.Join("..", "ca.crt"), wantErrText: "CA_FILE must not contain path traversal"},
		"a certificate file that isn't there": {ca: "ca.crt", cert: "nope.crt", key: "peer.key", wantErrText: "failed to load PEER_CERT_FILE"},
		"the key of another certificate":      {ca: "ca.crt", cert: "peer.crt", key: "foreign.key", wantErrText: "failed to load PEER_CERT_FILE"},
		"a certificate the CA didn't issue":   {ca: "ca.crt", cert: "foreign.crt", key: "foreign.key", wantErrText: "PEER_CERT_FILE, against CA_FILE"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			trust, err := loadPeerTrust(tt.ca, tt.cert, tt.key, tt.peers)

			if tt.wantErrText != "" {
				assert.ErrorContains(t, err, tt.wantErrText)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantCA, trust.HasCA())
			assert.Equal(t, tt.wantIdentity, trust.Identity())
		})
	}
}
