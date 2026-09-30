package credential

import "context"

var _ Keys = fakeKeys(nil)

// fakeKeys resolves kids to full key records, so a test can give a key a
// project and prefix. A nil value knows no keys.
type fakeKeys map[string]Key

func (f fakeKeys) Key(_ context.Context, kid string) (Key, error) {
	key, ok := f[kid]
	if !ok {
		return Key{}, errUnknownKey
	}
	return key, nil
}
