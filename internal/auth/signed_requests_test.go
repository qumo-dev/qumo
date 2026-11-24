package auth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/reqsign"
	"github.com/qumo-dev/qumo/internal/reqsign/reqsigntest"
	"github.com/qumo-dev/qumo/token"
)

// signedRequests records, for each request, its bearer and whether its
// signature verified as the control plane checks it (reqsigntest.Verify),
// over the body it arrived with.
type signedRequests struct {
	mu   sync.Mutex
	seen []signedRequest
}

type signedRequest struct {
	bearer    string
	signature string
	err       error // nil: the signature verified
}

func (s *signedRequests) record(r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err == nil {
		_, err = reqsigntest.Verify(r, body)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, signedRequest{bearer: r.Header.Get("Authorization"), signature: r.Header.Get(reqsign.HeaderSignature), err: err})
}

func (s *signedRequests) all() []signedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
}

func assertSigned(t *testing.T, got signedRequest, bearer string) {
	t.Helper()
	assert.NoError(t, got.err, "the signature verifies over the body sent")
	assert.Equal(t, "Bearer "+bearer, got.bearer, "the token is still sent beside the signature")
}

// With a signer, every key-set download is signed and still carries the token.
func TestVerifier_SignsItsRequests(t *testing.T) {
	var keyReqs signedRequests
	k := genKey(t, "acme/app")
	keys := keySetJSON(t, []token.SigningKey{k})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keyReqs.record(r)
		if _, err := w.Write(keys); err != nil {
			t.Errorf("write the key set: %v", err)
		}
	}))
	defer srv.Close()
	v, err := NewVerifier(VerifierConfig{Keys: srv.URL + "/keys", Token: "relay-token", Signer: reqsigntest.Signer(t)})
	require.NoError(t, err)
	var store keyStore

	require.NoError(t, v.source.refresh(t.Context(), &store, time.Now()))

	require.Len(t, keyReqs.all(), 1)
	assertSigned(t, keyReqs.all()[0], "relay-token")
}

// Without one, nothing is signed: the token alone, as before.
func TestVerifier_UnsignedWithoutASigner(t *testing.T) {
	var reqs signedRequests
	k := genKey(t, "acme/app")
	keys := keySetJSON(t, []token.SigningKey{k})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.record(r)
		if _, err := w.Write(keys); err != nil {
			t.Errorf("write the key set: %v", err)
		}
	}))
	defer srv.Close()
	v, err := NewVerifier(VerifierConfig{Keys: srv.URL, Token: "relay-token"})
	require.NoError(t, err)
	var store keyStore

	require.NoError(t, v.source.refresh(t.Context(), &store, time.Now()))

	require.Len(t, reqs.all(), 1)
	assert.Empty(t, reqs.all()[0].signature)
	assert.Equal(t, "Bearer relay-token", reqs.all()[0].bearer)
}
