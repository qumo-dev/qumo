package geodns

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
)

// maxGeoDBBytes bounds a downloaded location database, decompressed: a city
// database is a few hundred megabytes at most.
const maxGeoDBBytes = 1 << 30

// monthPlaceholder in a database URL is replaced with the month, "2006-01",
// for vendors that publish one file a month under a dated name.
const monthPlaceholder = "{month}"

// locator places an address on the globe with a MaxMind-format (.mmdb)
// city database. A zero locator, or one whose database does not know an
// address, places nothing: the client is then of unknown place.
type locator struct {
	mu sync.RWMutex
	db *maxminddb.Reader
}

var _ placer = (*locator)(nil)

// cityRecord is the part of a GeoIP2/GeoLite2 City record, a format other
// vendors' city databases share, that places an address.
type cityRecord struct {
	Location struct {
		Latitude  *float64 `maxminddb:"latitude"`
		Longitude *float64 `maxminddb:"longitude"`
	} `maxminddb:"location"`
}

// locate returns where addr is, or false.
func (l *locator) locate(addr netip.Addr) (location, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.db == nil || !addr.IsValid() {
		return location{}, false
	}
	var rec cityRecord
	if err := l.db.Lookup(addr.Unmap()).Decode(&rec); err != nil {
		return location{}, false
	}
	lat, lon := rec.Location.Latitude, rec.Location.Longitude
	if lat == nil || lon == nil || math.Abs(*lat) > 90 || math.Abs(*lon) > 180 {
		return location{}, false
	}
	return location{lat: *lat, lon: *lon}, true
}

// load opens the database at path and puts it in place of the one held.
func (l *locator) load(path string) error {
	db, err := maxminddb.Open(path)
	if err != nil {
		return fmt.Errorf("geodns: open the location database %s: %w", path, err)
	}
	l.mu.Lock()
	old := l.db
	l.db = db
	l.mu.Unlock()
	if old != nil {
		// No lookup holds it: they all run under the read lock.
		_ = old.Close() // not actionable: it is no longer read
	}
	return nil
}

// close releases the database.
func (l *locator) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.db != nil {
		_ = l.db.Close() // not actionable: the process is ending
		l.db = nil
	}
}

// errNotModified is a database that has not changed since the copy held.
var errNotModified = errors.New("geodns: the location database has not changed")

// geoFetcher keeps a location database at dest fresh from a URL.
type geoFetcher struct {
	urlTemplate string // may hold monthPlaceholder
	dest        string
	client      *http.Client
	now         func() time.Time
}

// urls returns the URLs to try, best first: this month's file, then last
// month's, which is what a vendor serves until it publishes the new one.
func (f *geoFetcher) urls() []string {
	if !strings.Contains(f.urlTemplate, monthPlaceholder) {
		return []string{f.urlTemplate}
	}
	now := f.now().UTC()
	this := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return []string{
		strings.ReplaceAll(f.urlTemplate, monthPlaceholder, this.Format("2006-01")),
		strings.ReplaceAll(f.urlTemplate, monthPlaceholder, this.AddDate(0, -1, 0).Format("2006-01")),
	}
}

// refresh downloads the database to dest when it is newer than the copy
// there. It returns errNotModified when the copy stands.
func (f *geoFetcher) refresh(ctx context.Context) error {
	var errs []error
	for _, u := range f.urls() {
		err := f.download(ctx, u)
		if err == nil || errors.Is(err, errNotModified) {
			return err
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// download fetches u into dest, through a file beside it that is checked to
// be a database before it takes dest's place.
func (f *geoFetcher) download(ctx context.Context, u string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("geodns: location database request: %w", err)
	}
	held, statErr := os.Stat(f.dest)
	if statErr == nil {
		req.Header.Set("If-Modified-Since", held.ModTime().UTC().Format(http.TimeFormat))
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("geodns: fetch the location database: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // not actionable: the body has been read
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		return errNotModified
	default:
		return fmt.Errorf("geodns: fetch the location database: %s answered %s", redactQuery(u), resp.Status)
	}

	var body io.Reader = resp.Body
	if parsed, err := url.Parse(u); err == nil && strings.HasSuffix(parsed.Path, ".gz") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return fmt.Errorf("geodns: decompress the location database: %w", err)
		}
		defer func() { _ = gz.Close() }() // not actionable: read to the end or failed
		body = gz
	}

	tmp, err := os.CreateTemp(filepath.Dir(f.dest), filepath.Base(f.dest)+".*.tmp")
	if err != nil {
		return fmt.Errorf("geodns: store the location database: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // not actionable: gone after the rename
	n, err := io.Copy(tmp, io.LimitReader(body, maxGeoDBBytes+1))
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("geodns: store the location database: %w", err)
	}
	if n > maxGeoDBBytes {
		return fmt.Errorf("geodns: the location database is larger than %d bytes", maxGeoDBBytes)
	}
	check, err := maxminddb.Open(tmp.Name())
	if err != nil {
		return fmt.Errorf("geodns: %s is not a location database: %w", redactQuery(u), err)
	}
	_ = check.Close() // not actionable: only opened to check the file
	if err := os.Rename(tmp.Name(), f.dest); err != nil {
		return fmt.Errorf("geodns: store the location database: %w", err)
	}
	return nil
}

// redactQuery drops a URL's query, where a vendor's licence key travels.
func redactQuery(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i] + "?…"
	}
	return u
}
