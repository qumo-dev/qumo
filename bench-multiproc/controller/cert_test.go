package controller

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qumo-dev/qumo/internal/devcert"
	"github.com/qumo-dev/qumo/internal/relay"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsurePeerCredentials(t *testing.T) {
	tests := map[string]struct {
		// existingCA is the validity of a CA already in the directory; zero
		// is an empty directory.
		existingCA  time.Duration
		withoutKey  bool // the CA's key file is missing
		wantKept    bool // the existing CA is the one used
		wantErrText string
	}{
		"an empty directory":                 {},
		"a CA with time left":                {existingCA: peerCAValidity, wantKept: true},
		"a CA that expires before its leafs": {existingCA: time.Hour},
		"a CA certificate without its key":   {existingCA: peerCAValidity, withoutKey: true, wantErrText: "is missing"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			var existing []byte
			if tt.existingCA != 0 {
				ca, err := devcert.NewCA("existing relay CA", tt.existingCA)
				require.NoError(t, err)
				existing = ca.CertPEM()
				keyPEM, err := ca.KeyPEM()
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(dir, peerCAFile), existing, 0o600))
				if !tt.withoutKey {
					require.NoError(t, os.WriteFile(filepath.Join(dir, peerCAKey), keyPEM, 0o600))
				}
			}

			err := EnsurePeerCredentials(dir, []string{"hub-1", "edge-1"})

			if tt.wantErrText != "" {
				assert.ErrorContains(t, err, tt.wantErrText)
				return
			}
			require.NoError(t, err)
			used, err := os.ReadFile(filepath.Join(dir, peerCAFile))
			require.NoError(t, err)
			assert.Equal(t, tt.wantKept, string(used) == string(existing), "whether the existing CA was kept")
			// What the relays are started with must be what a relay accepts.
			// The CA path is relative to the cell's directory, where they run.
			for _, relayName := range []string{"hub-1", "edge-1"} {
				caFile, certFile, keyFile := PeerCredentialPaths(dir, relayName)
				caPEM, err := os.ReadFile(filepath.Join(dir, caFile))
				require.NoError(t, err)
				cert, err := tls.LoadX509KeyPair(certFile, keyFile)
				require.NoError(t, err)
				trust, err := relay.NewPeerTrust(caPEM, &cert)
				require.NoError(t, err, "the relay accepts %s's credentials", relayName)
				assert.Equal(t, relayName, trust.Identity())
			}
		})
	}
}
