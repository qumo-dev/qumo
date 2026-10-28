package geodns

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"
)

// maxFeedBytes bounds a feed body: a feed lists relays, not a database.
const maxFeedBytes = 1 << 20

// errNoRegion is a feed that names no region with an address: never served,
// since it would take every relay out of DNS on the word of one response.
var errNoRegion = errors.New("geodns: the feed names no region with a usable address")

// feedBody is the feed's wire form ("The feed" in docs/site/content/en/docs/cli/dns.md):
//
//	{"regions": [{"region": "a", "lat": 35.7, "lon": 139.7, "addrs": ["192.0.2.1"]}],
//	 "published_at": "2026-01-01T00:00:00Z"}
type feedBody struct {
	Regions     []feedRegion `json:"regions"`
	PublishedAt time.Time    `json:"published_at"`
}

type feedRegion struct {
	Region string   `json:"region"`
	Lat    float64  `json:"lat"`
	Lon    float64  `json:"lon"`
	Addrs  []string `json:"addrs"`
}

// region is a place clients are sent to, and the addresses there.
type region struct {
	name  string
	at    location
	addrs []netip.Addr
}

// zone is one reading of the feed: what the served name answers. It is never
// changed once built, so a query reads it without a lock.
type zone struct {
	regions []region // by name
	serial  uint32   // the SOA serial: when the feed was published
}

// parseFeed reads a feed body into a zone. Entries it cannot use (a bad
// address, a place off the globe) are left out and reported; a feed left with
// no region is errNoRegion.
func parseFeed(body []byte) (*zone, []string, error) {
	var f feedBody
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, nil, fmt.Errorf("geodns: decode the feed: %w", err)
	}
	var skipped []string
	z := &zone{serial: serialOf(f.PublishedAt)}
	for _, r := range f.Regions {
		if r.Region == "" || math.Abs(r.Lat) > 90 || math.Abs(r.Lon) > 180 {
			skipped = append(skipped, fmt.Sprintf("region %q: no name, or a place off the globe", r.Region))
			continue
		}
		var addrs []netip.Addr
		for _, raw := range r.Addrs {
			addr, err := netip.ParseAddr(raw)
			if err != nil || !addr.IsGlobalUnicast() || addr.Unmap().IsPrivate() {
				skipped = append(skipped, fmt.Sprintf("region %q: address %q is not a public unicast address", r.Region, raw))
				continue
			}
			addrs = append(addrs, addr.Unmap())
		}
		if len(addrs) == 0 {
			continue
		}
		slices.SortFunc(addrs, netip.Addr.Compare)
		z.regions = append(z.regions, region{name: r.Region, at: location{lat: r.Lat, lon: r.Lon}, addrs: slices.Compact(addrs)})
	}
	if len(z.regions) == 0 {
		return nil, skipped, errNoRegion
	}
	slices.SortFunc(z.regions, func(a, b region) int { return strings.Compare(a.name, b.name) })
	return z, skipped, nil
}

// serialOf is the SOA serial for a feed published at t: its Unix time, which
// grows with every publication until 2106.
func serialOf(t time.Time) uint32 {
	if t.IsZero() || t.Unix() <= 0 || t.Unix() > math.MaxUint32 {
		return 1
	}
	return uint32(t.Unix())
}

// errFeedNotReady is a feed that has nothing yet (503): what is held stays.
var errFeedNotReady = errors.New("geodns: the feed has nothing to serve yet")

// feedClient reads the feed.
type feedClient struct {
	url    string
	token  string // sent as a bearer token when set
	client *http.Client
}

// fetch reads the feed once.
func (c *feedClient) fetch(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, fmt.Errorf("geodns: feed request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("geodns: fetch the feed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // not actionable: the body has been read
	switch {
	case resp.StatusCode == http.StatusServiceUnavailable:
		return nil, errFeedNotReady
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("geodns: the feed answered %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes+1))
	if err != nil {
		return nil, fmt.Errorf("geodns: read the feed: %w", err)
	}
	if len(body) > maxFeedBytes {
		return nil, fmt.Errorf("geodns: the feed is larger than %d bytes", maxFeedBytes)
	}
	return body, nil
}

// writeFileAtomic writes data to name in dir through a file beside it, so
// that a reader (or a restart) sees the old content or the new, never part
// of one.
func writeFileAtomic(dir *os.Root, name string, data []byte) error {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("geodns: write %s: %w", name, err)
	}
	tmpName := name + "." + hex.EncodeToString(suffix[:]) + ".tmp"
	tmp, err := dir.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("geodns: write %s: %w", name, err)
	}
	defer func() { _ = dir.Remove(tmpName) }() // not actionable: gone after the rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close() // not actionable: the write already failed
		return fmt.Errorf("geodns: write %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("geodns: write %s: %w", name, err)
	}
	if err := dir.Rename(tmpName, name); err != nil {
		return fmt.Errorf("geodns: write %s: %w", name, err)
	}
	return nil
}
