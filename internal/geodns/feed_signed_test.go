package geodns

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/relaysign"
)

// With the node relay's peer identity, each feed read is signed, and the
// token still sent (internal/relaysign checks the signature itself).
func TestFeedClient_Fetch_Signed(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "qumo-relay-tk1a-1"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	signer, err := relaysign.New(tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key})
	require.NoError(t, err)
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := &feedClient{url: srv.URL, token: "secret", signer: signer, client: srv.Client()}

	_, err = c.fetch(t.Context())

	require.NoError(t, err)
	assert.NotEmpty(t, got.Get(relaysign.HeaderCertificate))
	assert.NotEmpty(t, got.Get(relaysign.HeaderTimestamp))
	assert.NotEmpty(t, got.Get(relaysign.HeaderSignature))
	assert.Equal(t, "Bearer secret", got.Get("Authorization"))
}
