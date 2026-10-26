package geodns

import (
	"cmp"
	"math"
	"net/netip"
	"slices"
)

// location is a point on the globe, in degrees.
type location struct {
	lat, lon float64
}

// distanceKM is the great-circle distance between two points (haversine).
func distanceKM(a, b location) float64 {
	const earthRadiusKM = 6371
	rad := func(deg float64) float64 { return deg * math.Pi / 180 }
	dLat, dLon := rad(b.lat-a.lat), rad(b.lon-a.lon)
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(rad(a.lat))*math.Cos(rad(b.lat))*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusKM * math.Asin(math.Sqrt(h))
}

// pick returns up to limit addresses of one family (v6 for AAAA) for a client
// at (nil when unknown): the nearest region's first, then the next region's,
// so that a client that tries another address tries another place.
//
// turn rotates the addresses within each region, so that clients of one
// place spread over its relays. A client of unknown place gets the regions
// in name order, rotated by turn too.
func (z *zone) pick(at *location, v6 bool, limit int, turn uint64) []netip.Addr {
	regions := slices.Clone(z.regions)
	if at != nil {
		slices.SortStableFunc(regions, func(a, b region) int {
			return cmp.Compare(distanceKM(*at, a.at), distanceKM(*at, b.at))
		})
	} else if len(regions) > 0 {
		k := int(turn % uint64(len(regions)))
		regions = slices.Concat(regions[k:], regions[:k])
	}
	var out []netip.Addr
	for _, r := range regions {
		var family []netip.Addr
		for _, a := range r.addrs {
			if a.Is6() == v6 {
				family = append(family, a)
			}
		}
		if len(family) == 0 {
			continue
		}
		k := int(turn % uint64(len(family)))
		for _, a := range slices.Concat(family[k:], family[:k]) {
			if len(out) == limit {
				return out
			}
			out = append(out, a)
		}
	}
	return out
}
