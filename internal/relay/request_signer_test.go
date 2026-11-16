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
// with; with none it signs nothing. A key that can't sign is an error when
// the relay calls a control plane, and only a warning when it does not.
func TestRequestSigner(t *testing.T) {
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	edPair := reqsigntest.Pair(t)
	edPair.PrivateKey = ed
	ecdsaPair := reqsigntest.Pair(t)
	tests := map[string]struct {
		trust             *PeerTrust
		callsControlPlane bool
		wantSigner        bool
		wantErr           bool
	}{
		"no trust":               {callsControlPlane: true},
		"a CA, no peer identity": {trust: &PeerTrust{}, callsControlPlane: true},
		"a peer identity":        {trust: &PeerTrust{cert: &ecdsaPair}, callsControlPlane: true, wantSigner: true},
		"a key that is not ECDSA, with a control plane": {trust: &PeerTrust{cert: &edPair}, callsControlPlane: true, wantErr: true},
		"a key that is not ECDSA, peering only":         {trust: &PeerTrust{cert: &edPair}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			signer, err := requestSigner(tt.trust, tt.callsControlPlane)

			if tt.wantErr {
				assert.ErrorContains(t, err, "not ECDSA")
				return
			}
			require.NoError(t, err)
			if !tt.wantSigner {
				assert.Nil(t, signer)
				return
			}
			require.NotNil(t, signer)
			req := httptest.NewRequest(http.MethodGet, "/v1/relays/trust", nil)
			require.NoError(t, signer.Sign(req, nil))
			_, err = reqsigntest.Verify(req, nil)
			assert.NoError(t, err)
		})
	}
}
