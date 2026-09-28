package relay

import (
	"context"
	"crypto/ed25519"
)

var _ signingKeys = (*fakeSigningKeys)(nil)

// fakeSigningKeys resolves kids from a fixed map. A zero value knows no keys;
// err, when set, is returned for every lookup (e.g. errKeysStale).
type fakeSigningKeys struct {
	keys map[string]ed25519.PublicKey
	err  error
}

func (f *fakeSigningKeys) key(_ context.Context, kid string) (ed25519.PublicKey, error) {
	if f.err != nil {
		return nil, f.err
	}
	pub, ok := f.keys[kid]
	if !ok {
		return nil, errUnknownKey
	}
	return pub, nil
}
