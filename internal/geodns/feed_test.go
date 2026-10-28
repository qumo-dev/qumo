package geodns

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFeed(t *testing.T) {
	tests := map[string]struct {
		body       string
		wantErr    error
		wantDecode bool
		regions    []string
		addrs      int
		skipped    int
		serial     uint32
	}{
		"two regions, sorted by name": {
			body:    `{"regions":[{"region":"east","lat":35.68,"lon":139.69,"addrs":["192.0.2.20"]},{"region":"north","lat":43.06,"lon":141.35,"addrs":["192.0.2.10","2001:db8::10"]}],"published_at":"2026-01-02T03:04:05Z"}`,
			regions: []string{"east", "north"},
			addrs:   3,
			serial:  uint32(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Unix()),
		},
		"an unusable address is left out": {
			body:    `{"regions":[{"region":"east","lat":35.68,"lon":139.69,"addrs":["192.0.2.20","nope","10.0.0.1","127.0.0.1"]}]}`,
			regions: []string{"east"},
			addrs:   1,
			skipped: 3,
			serial:  1,
		},
		"a duplicate address is kept once": {
			body:    `{"regions":[{"region":"east","lat":35.68,"lon":139.69,"addrs":["192.0.2.20","192.0.2.20"]}]}`,
			regions: []string{"east"},
			addrs:   1,
			serial:  1,
		},
		"no address that is not public unicast is served": {
			body:    `{"regions":[{"region":"east","lat":35.68,"lon":139.69,"addrs":["192.0.2.20","0.0.0.0","224.0.0.1","169.254.0.1","::","::1","fe80::1","fc00::1","ff02::1"]}]}`,
			regions: []string{"east"},
			addrs:   1,
			skipped: 8,
			serial:  1,
		},
		"a region with no name, or a longitude off the globe, is left out": {
			body:    `{"regions":[{"region":"","lat":0,"lon":0,"addrs":["192.0.2.1"]},{"region":"x","lat":0,"lon":-181,"addrs":["192.0.2.2"]},{"region":"east","lat":35.68,"lon":139.69,"addrs":["192.0.2.20"]}]}`,
			regions: []string{"east"},
			addrs:   1,
			skipped: 2,
			serial:  1,
		},
		"the edges of the globe are on it": {
			body:    `{"regions":[{"region":"pole","lat":-90,"lon":180,"addrs":["192.0.2.1"]}]}`,
			regions: []string{"pole"},
			addrs:   1,
			serial:  1,
		},
		"fields it does not know are ignored": {
			body:    `{"regions":[{"region":"east","lat":35.68,"lon":139.69,"addrs":["192.0.2.20"],"weight":3}],"version":2}`,
			regions: []string{"east"},
			addrs:   1,
			serial:  1,
		},
		"a region off the globe is left out": {
			body:    `{"regions":[{"region":"x","lat":91,"lon":0,"addrs":["192.0.2.1"]},{"region":"east","lat":35.68,"lon":139.69,"addrs":["192.0.2.20"]}]}`,
			regions: []string{"east"},
			addrs:   1,
			skipped: 1,
			serial:  1,
		},
		"no usable region": {
			body:    `{"regions":[{"region":"east","lat":35.68,"lon":139.69,"addrs":[]}]}`,
			wantErr: errNoRegion,
		},
		"no region at all": {
			body:    `{"regions":[]}`,
			wantErr: errNoRegion,
		},
		"not JSON": {
			body:       `<html>`,
			wantDecode: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			z, skipped, err := parseFeed([]byte(tt.body))
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			if tt.wantDecode {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			var names []string
			addrs := 0
			for _, r := range z.regions {
				names = append(names, r.name)
				addrs += len(r.addrs)
			}
			assert.Equal(t, tt.regions, names)
			assert.Equal(t, tt.addrs, addrs)
			assert.Len(t, skipped, tt.skipped)
			assert.Equal(t, tt.serial, z.serial)
		})
	}
}

func TestParseFeed_UnmapsIPv4InIPv6(t *testing.T) {
	z, _, err := parseFeed([]byte(`{"regions":[{"region":"east","lat":35.68,"lon":139.69,"addrs":["::ffff:192.0.2.20"]}]}`))
	require.NoError(t, err)
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("192.0.2.20")}, z.regions[0].addrs)
}

func TestSerialOf(t *testing.T) {
	tests := map[string]struct {
		at       time.Time
		expected uint32
	}{
		"zero":         {at: time.Time{}, expected: 1},
		"before 1970":  {at: time.Date(1969, 1, 1, 0, 0, 0, 0, time.UTC), expected: 1},
		"after 2106":   {at: time.Date(2107, 1, 1, 0, 0, 0, 0, time.UTC), expected: 1},
		"a usual time": {at: time.Unix(1_800_000_000, 0), expected: 1_800_000_000},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.expected, serialOf(tt.at))
		})
	}
}

func TestFeedClient_Fetch(t *testing.T) {
	tests := map[string]struct {
		status   int
		body     string
		token    string
		wantErr  error
		wantFail bool
	}{
		"ok, with the token":  {status: http.StatusOK, body: `{"regions":[]}`, token: "secret"},
		"ok, without a token": {status: http.StatusOK, body: `{}`},
		"not ready":           {status: http.StatusServiceUnavailable, wantErr: errFeedNotReady},
		"refused":             {status: http.StatusUnauthorized, wantFail: true},
		"too large":           {status: http.StatusOK, body: strings.Repeat("x", maxFeedBytes+1), wantFail: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			c := &feedClient{url: srv.URL, token: tt.token, client: srv.Client()}
			body, err := c.fetch(ctx)
			switch {
			case tt.wantErr != nil:
				assert.ErrorIs(t, err, tt.wantErr)
			case tt.wantFail:
				assert.Error(t, err)
			default:
				require.NoError(t, err)
				assert.Equal(t, tt.body, string(body))
			}
			if tt.token != "" {
				assert.Equal(t, "Bearer "+tt.token, gotAuth)
			} else {
				assert.Empty(t, gotAuth)
			}
		})
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer func() { _ = root.Close() }() // not actionable: a test's temporary directory
	require.NoError(t, writeFileAtomic(root, "feed.json", []byte("one")))
	require.NoError(t, writeFileAtomic(root, "feed.json", []byte("two")))

	got, err := root.ReadFile("feed.json")
	require.NoError(t, err)
	assert.Equal(t, "two", string(got))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temporary file is left behind")
}

func TestWriteFileAtomic_OutsideTheDirectory(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	defer func() { _ = root.Close() }() // not actionable: a test's temporary directory

	assert.Error(t, writeFileAtomic(root, "../feed.json", []byte("x")))
	assert.Error(t, writeFileAtomic(root, "no/such/feed.json", []byte("x")))
}
