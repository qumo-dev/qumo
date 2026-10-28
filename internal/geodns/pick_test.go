package geodns

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
)

var (
	tokyo     = location{lat: 35.68, lon: 139.69}
	sapporo   = location{lat: 43.06, lon: 141.35}
	asahikawa = location{lat: 43.77, lon: 142.37}
	osaka     = location{lat: 34.69, lon: 135.50}
)

// testZone is two regions, in name order as parseFeed leaves them: "east"
// with two IPv4 addresses, "north" with one address of each family.
func testZone() *zone {
	return &zone{serial: 7, regions: []region{
		{name: "east", at: tokyo, addrs: []netip.Addr{netip.MustParseAddr("192.0.2.20"), netip.MustParseAddr("192.0.2.21")}},
		{name: "north", at: sapporo, addrs: []netip.Addr{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("2001:db8::10")}},
	}}
}

func TestDistanceKM(t *testing.T) {
	// Tokyo to Sapporo is about 830 km as the crow flies.
	assert.InDelta(t, 830, distanceKM(tokyo, sapporo), 20)
	assert.InDelta(t, 0, distanceKM(tokyo, tokyo), 0.001)
}

func TestZone_Pick(t *testing.T) {
	addrs := func(s ...string) []netip.Addr {
		out := make([]netip.Addr, len(s))
		for i, a := range s {
			out[i] = netip.MustParseAddr(a)
		}
		return out
	}
	tests := map[string]struct {
		at       *location
		v6       bool
		limit    int
		turn     uint64
		expected []netip.Addr
	}{
		"a client in Asahikawa gets the north, then the east": {
			at: &asahikawa, limit: 2, expected: addrs("192.0.2.10", "192.0.2.20"),
		},
		"a client in Osaka gets both relays in the east first": {
			at: &osaka, limit: 2, expected: addrs("192.0.2.20", "192.0.2.21"),
		},
		"the turn rotates within the nearest region": {
			at: &osaka, limit: 1, turn: 1, expected: addrs("192.0.2.21"),
		},
		"the limit caps the answer": {
			at: &osaka, limit: 3, expected: addrs("192.0.2.20", "192.0.2.21", "192.0.2.10"),
		},
		"IPv6 comes only from regions that have it": {
			at: &osaka, v6: true, limit: 2, expected: addrs("2001:db8::10"),
		},
		"an unknown client gets the regions in name order on turn 0": {
			limit: 1, expected: addrs("192.0.2.20"),
		},
		"an unknown client gets the next region on turn 1": {
			limit: 1, turn: 1, expected: addrs("192.0.2.10"),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.expected, testZone().pick(tt.at, tt.v6, tt.limit, tt.turn))
		})
	}
}

// TestZone_Pick_ATieKeepsNameOrder: two regions as near are taken in name
// order, so that every server answers a client alike.
func TestZone_Pick_ATieKeepsNameOrder(t *testing.T) {
	z := &zone{regions: []region{
		{name: "a", at: tokyo, addrs: []netip.Addr{netip.MustParseAddr("192.0.2.1")}},
		{name: "b", at: tokyo, addrs: []netip.Addr{netip.MustParseAddr("192.0.2.2")}},
	}}

	for turn := range uint64(3) {
		assert.Equal(t, []netip.Addr{netip.MustParseAddr("192.0.2.1")}, z.pick(&osaka, false, 1, turn))
	}
}

// TestZone_Pick_SpreadsOverARegion: over as many queries as a region has
// relays, each relay is answered first once.
func TestZone_Pick_SpreadsOverARegion(t *testing.T) {
	z := manyAddrs(3)
	first := map[netip.Addr]int{}

	for turn := range uint64(3) {
		first[z.pick(&osaka, false, 1, turn)[0]]++
	}

	assert.Len(t, first, 3)
}

func TestZone_Pick_LeavesZoneUnchanged(t *testing.T) {
	z := testZone()
	_ = z.pick(&osaka, false, 3, 1) // the result is not under test
	assert.Equal(t, testZone(), z)
}
