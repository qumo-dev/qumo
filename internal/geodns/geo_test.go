package geodns

import (
	"bytes"
	"compress/gzip"
	"context"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testCityDB is a city database: 198.51.100.0/24 in Asahikawa, 203.0.113.0/24
// with a city record but no location.
func testCityDB(tb testing.TB) []byte {
	tb.Helper()
	w, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: "GeoIP2-City", IncludeReservedNetworks: true, RecordSize: 24})
	require.NoError(tb, err)
	insert := func(cidr string, rec mmdbtype.Map) {
		_, network, err := net.ParseCIDR(cidr)
		require.NoError(tb, err)
		require.NoError(tb, w.Insert(network, rec))
	}
	insert("198.51.100.0/24", mmdbtype.Map{"location": mmdbtype.Map{
		"latitude": mmdbtype.Float64(asahikawa.lat), "longitude": mmdbtype.Float64(asahikawa.lon),
	}})
	insert("203.0.113.0/24", mmdbtype.Map{"city": mmdbtype.Map{"geoname_id": mmdbtype.Uint32(1)}})
	var buf bytes.Buffer
	_, err = w.WriteTo(&buf)
	require.NoError(tb, err)
	return buf.Bytes()
}

func TestLocator_Locate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "city.mmdb")
	require.NoError(t, os.WriteFile(path, testCityDB(t), 0o600))
	l := &locator{}
	require.NoError(t, l.load(path))
	defer l.close()

	tests := map[string]struct {
		addr     netip.Addr
		expected location
		found    bool
	}{
		"placed":                 {addr: netip.MustParseAddr("198.51.100.7"), expected: asahikawa, found: true},
		"IPv4 in IPv6 is placed": {addr: netip.MustParseAddr("::ffff:198.51.100.7"), expected: asahikawa, found: true},
		"a record with no place": {addr: netip.MustParseAddr("203.0.113.1")},
		"not in the database":    {addr: netip.MustParseAddr("192.0.2.1")},
		"no address":             {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			loc, ok := l.locate(tt.addr)
			assert.Equal(t, tt.found, ok)
			if tt.found {
				assert.InDelta(t, tt.expected.lat, loc.lat, 0.0001)
				assert.InDelta(t, tt.expected.lon, loc.lon, 0.0001)
			}
		})
	}
}

func TestLocator_Locate_Empty(t *testing.T) {
	_, ok := (&locator{}).locate(netip.MustParseAddr("198.51.100.7"))
	assert.False(t, ok)
}

func TestLocator_Load_NotADatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "city.mmdb")
	require.NoError(t, os.WriteFile(path, []byte("not a database"), 0o600))
	assert.Error(t, (&locator{}).load(path))
}

func TestGeoFetcher_URLs(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC) }
	tests := map[string]struct {
		template string
		expected []string
	}{
		"dated: this month, then last month across a year": {
			template: "https://db.example/city-{month}.mmdb.gz",
			expected: []string{"https://db.example/city-2026-01.mmdb.gz", "https://db.example/city-2025-12.mmdb.gz"},
		},
		"undated": {
			template: "https://db.example/city.mmdb",
			expected: []string{"https://db.example/city.mmdb"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := &geoFetcher{urlTemplate: tt.template, now: now}
			assert.Equal(t, tt.expected, f.urls())
		})
	}
}

func gzipped(tb testing.TB, data []byte) []byte {
	tb.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, err := gz.Write(data)
	require.NoError(tb, err)
	require.NoError(tb, gz.Close())
	return buf.Bytes()
}

func TestGeoFetcher_Refresh(t *testing.T) {
	db := testCityDB(t)
	tests := map[string]struct {
		files    map[string][]byte // path -> body served; other paths are 404
		template string
		held     []byte // what dest holds before, or nil
		wantErr  bool
		expected []byte // what dest holds after
	}{
		"gzipped, this month": {
			files:    map[string][]byte{"/city-2026-01.mmdb.gz": gzipped(t, db)},
			template: "/city-{month}.mmdb.gz",
			expected: db,
		},
		"last month while this month is not out": {
			files:    map[string][]byte{"/city-2025-12.mmdb": db},
			template: "/city-{month}.mmdb",
			expected: db,
		},
		"not a database: the held copy stands": {
			files:    map[string][]byte{"/city.mmdb": []byte("garbage")},
			template: "/city.mmdb",
			held:     []byte("held"),
			wantErr:  true,
			expected: []byte("held"),
		},
		"a broken gzip: the held copy stands": {
			files:    map[string][]byte{"/city.mmdb.gz": []byte("not gzip")},
			template: "/city.mmdb.gz",
			held:     []byte("held"),
			wantErr:  true,
			expected: []byte("held"),
		},
		"a newer database replaces the held copy": {
			files:    map[string][]byte{"/city.mmdb": db},
			template: "/city.mmdb",
			held:     []byte("held"),
			expected: db,
		},
		"nothing found": {
			template: "/city-{month}.mmdb",
			wantErr:  true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, ok := tt.files[r.URL.Path]
				if !ok {
					http.NotFound(w, r)
					return
				}
				_, _ = w.Write(body)
			}))
			defer srv.Close()
			dir := t.TempDir()
			dest := filepath.Join(dir, "city.mmdb")
			if tt.held != nil {
				require.NoError(t, os.WriteFile(dest, tt.held, 0o600))
			}
			f := &geoFetcher{
				urlTemplate: srv.URL + tt.template,
				dest:        dest,
				client:      srv.Client(),
				now:         func() time.Time { return time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC) },
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			err := f.refresh(ctx)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			got, readErr := fs.ReadFile(os.DirFS(dir), "city.mmdb")
			if tt.expected == nil {
				assert.ErrorIs(t, readErr, os.ErrNotExist)
				return
			}
			require.NoError(t, readErr)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestGeoFetcher_Refresh_NotModified(t *testing.T) {
	var gotSince string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSince = r.Header.Get("If-Modified-Since")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "city.mmdb")
	require.NoError(t, os.WriteFile(dest, []byte("held"), 0o600))
	f := &geoFetcher{urlTemplate: srv.URL + "/city.mmdb", dest: dest, client: srv.Client(), now: time.Now}

	err := f.refresh(t.Context())

	assert.ErrorIs(t, err, errNotModified)
	assert.NotEmpty(t, gotSince)
}

// TestGeoFetcher_Refresh_LastMonthOnAServerError: this month's file failing
// in any way, not only 404, falls back to last month's.
func TestGeoFetcher_Refresh_LastMonthOnAServerError(t *testing.T) {
	db := testCityDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/city-2026-01.mmdb" {
			http.Error(w, "busy", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(db) // not actionable: a test server
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "city.mmdb")
	f := &geoFetcher{
		urlTemplate: srv.URL + "/city-{month}.mmdb", dest: dest, client: srv.Client(),
		now: func() time.Time { return time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC) },
	}

	require.NoError(t, f.refresh(t.Context()))

	got, err := fs.ReadFile(os.DirFS(filepath.Dir(dest)), "city.mmdb")
	require.NoError(t, err)
	assert.Equal(t, db, got)
}

// TestLocator_LoadWhileLocating swaps the database while lookups run; run
// with -race.
func TestLocator_LoadWhileLocating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "city.mmdb")
	require.NoError(t, os.WriteFile(path, testCityDB(t), 0o600))
	l := &locator{}
	require.NoError(t, l.load(path))
	defer l.close()

	var wg sync.WaitGroup
	wg.Go(func() {
		for range 20 {
			assert.NoError(t, l.load(path))
		}
	})
	for range 4 {
		wg.Go(func() {
			for range 200 {
				_, ok := l.locate(netip.MustParseAddr("198.51.100.7"))
				assert.True(t, ok)
			}
		})
	}
	wg.Wait()
}

func TestRedactQuery(t *testing.T) {
	assert.Equal(t, "https://db.example/city.mmdb?…", redactQuery("https://db.example/city.mmdb?license_key=secret"))
	assert.Equal(t, "https://db.example/city.mmdb", redactQuery("https://db.example/city.mmdb"))
}
