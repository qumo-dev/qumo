package relay

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/reqsign/reqsigntest"
)

// The relay signs its control-plane requests with the identity it peers
// with, and with nothing when it has none or its key can't sign.
func TestRequestSigner(t *testing.T) {
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	edPair := reqsigntest.Pair(t)
	edPair.PrivateKey = ed
	ecdsaPair := reqsigntest.Pair(t)
	tests := map[string]struct {
		trust      *PeerTrust
		wantSigner bool
	}{
		"no trust":                {},
		"a CA, no peer identity":  {trust: &PeerTrust{}},
		"a peer identity":         {trust: &PeerTrust{cert: &ecdsaPair}, wantSigner: true},
		"a key that is not ECDSA": {trust: &PeerTrust{cert: &edPair}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			signer := requestSigner(tt.trust)

			if !tt.wantSigner {
				assert.Nil(t, signer)
				return
			}
			require.NotNil(t, signer)
			req := httptest.NewRequest(http.MethodGet, "/v1/relays/trust", nil)
			require.NoError(t, signer.Sign(req, nil))
			_, err := reqsigntest.Verify(req, nil)
			assert.NoError(t, err)
		})
	}
}
