package geodns

import "net/netip"

// fakeLocator places the addresses in at; any other is of unknown place.
type fakeLocator struct {
	at map[netip.Addr]location
}

var _ placer = (*fakeLocator)(nil)

func (f *fakeLocator) locate(addr netip.Addr) (location, bool) {
	loc, ok := f.at[addr]
	return loc, ok
}
