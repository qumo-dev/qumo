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
// at (nil when unknown), all of one region: the nearest that has an address
// of that family. Resolvers and clients reorder the addresses of an answer,
// so an answer that mixed regions would send some clients to the far one.
//
// turn rotates the addresses within the region, so that clients of one
// place spread over its relays. A client of unknown place gets the regions
// in name order, in turn, and each region's addresses in turn too.
func (z *zone) pick(at *location, v6 bool, limit int, turn uint64) []netip.Addr {
	var candidates []region
	for _, r := range z.regions {
		var family []netip.Addr
		for _, a := range r.addrs {
			if a.Is6() == v6 {
				family = append(family, a)
			}
		}
		if len(family) > 0 {
			candidates = append(candidates, region{name: r.name, at: r.at, addrs: family})
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	var chosen region
	if at != nil {
		// The first of the nearest: a tie goes by name, alike on every server.
		chosen = slices.MinFunc(candidates, func(a, b region) int {
			return cmp.Compare(distanceKM(*at, a.at), distanceKM(*at, b.at))
		})
	} else {
		n := uint64(len(candidates))
		chosen = candidates[turn%n]
		turn /= n // what is left rotates the region's addresses
	}
	k := int(turn % uint64(len(chosen.addrs)))
	out := slices.Concat(chosen.addrs[k:], chosen.addrs[:k])
	return out[:min(limit, len(out))]
}
