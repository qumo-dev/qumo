package relay

import (
	"context"
	"crypto/ed25519"
	"errors"

	"github.com/qumo-dev/qumo/internal/credential"
)

var _ credential.Keys = (*fakeCredentialKeys)(nil)

// fakeCredentialKeys resolves kids from a fixed map, standing in for the
// control plane's JWKS. A zero value knows no keys.
type fakeCredentialKeys struct {
	keys map[string]ed25519.PublicKey
}

func (f *fakeCredentialKeys) Key(_ context.Context, kid string) (ed25519.PublicKey, error) {
	pub, ok := f.keys[kid]
	if !ok {
		return nil, errors.New("fake keys: unknown kid")
	}
	return pub, nil
}
