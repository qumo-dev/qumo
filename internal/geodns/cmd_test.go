package geodns

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setRequired sets the settings Run cannot do without.
func setRequired(tb testing.TB) {
	tb.Helper()
	tb.Setenv("QUMO_DNS_ZONE", "Geo.Example.com")
	tb.Setenv("QUMO_DNS_FEED_URL", "https://cp.example.com/feed")
	tb.Setenv("QUMO_DNS_NAMESERVERS", "ns1.example.com, ns2.example.com.")
}

func TestConfigFromEnv_Defaults(t *testing.T) {
	setRequired(t)

	cfg, err := configFromEnv()

	require.NoError(t, err)
	assert.Equal(t, "geo.example.com.", cfg.zone)
	assert.Equal(t, "geo.example.com.", cfg.name, "the zone itself by default")
	assert.Equal(t, []string{"ns1.example.com.", "ns2.example.com."}, cfg.nameservers)
	assert.Equal(t, "hostmaster.geo.example.com.", cfg.hostmaster)
	assert.Equal(t, defaultListen, cfg.listen)
	assert.Equal(t, defaultHTTPAddr, cfg.httpAddr)
	assert.Equal(t, defaultTTL, cfg.ttl)
	assert.Equal(t, defaultMaxAnswers, cfg.maxAnswers)
	assert.Equal(t, defaultFeedInterval, cfg.feedInterval)
	assert.Equal(t, defaultGeoInterval, cfg.geoInterval)
	assert.Empty(t, cfg.geoDB)
}

func TestConfigFromEnv_GeoURLKeepsTheDatabaseInTheStateDir(t *testing.T) {
	setRequired(t)
	dir := t.TempDir()
	t.Setenv("QUMO_DNS_STATE_DIR", dir)
	t.Setenv("QUMO_DNS_GEOIP_URL", "https://db.example/city-{month}.mmdb.gz")

	cfg, err := configFromEnv()

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, geoFile), cfg.geoDB)
}

func TestConfigFromEnv_Errors(t *testing.T) {
	tests := map[string]struct {
		env      map[string]string
		required bool
	}{
		"nothing set": {},
		"a name outside the zone": {
			env: map[string]string{"QUMO_DNS_NAME": "cdn.example.org"}, required: true,
		},
		"a database URL with nowhere to keep it": {
			env: map[string]string{"QUMO_DNS_GEOIP_URL": "https://db.example/city.mmdb"}, required: true,
		},
		"no answers": {
			env: map[string]string{"QUMO_DNS_MAX_ANSWERS": "0"}, required: true,
		},
		"a TTL of zero": {
			env: map[string]string{"QUMO_DNS_TTL": "0s"}, required: true,
		},
		"a bad interval": {
			env: map[string]string{"QUMO_DNS_FEED_INTERVAL": "often"}, required: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			for _, key := range []string{"QUMO_DNS_ZONE", "QUMO_DNS_FEED_URL", "QUMO_DNS_NAMESERVERS"} {
				t.Setenv(key, "")
			}
			if tt.required {
				setRequired(t)
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			_, err := configFromEnv()
			assert.Error(t, err)
		})
	}
}

const testFeed = `{"regions":[{"region":"east","lat":35.68,"lon":139.69,"addrs":["192.0.2.20"]}],"published_at":"2026-01-02T03:04:05Z"}`

// TestService_FeedRound reads the feed, keeps it on disk, keeps serving it
// when the feed fails, and serves it after a restart without the feed.
func TestService_FeedRound(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(testFeed))
	}))
	defer srv.Close()
	dir := t.TempDir()
	cfg := config{
		zone: "geo.example.com.", name: "cdn.geo.example.com.", nameservers: []string{"ns1.example.com."},
		hostmaster: "hostmaster.geo.example.com.", ttl: 30 * time.Second, maxAnswers: 2,
		feedURL: srv.URL, feedInterval: time.Hour, stateDir: dir,
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	s, err := newService(cfg, prometheus.NewRegistry())
	require.NoError(t, err)
	defer s.close()
	require.Nil(t, s.auth.current.Load())
	s.feedRound(ctx)
	require.NotNil(t, s.auth.current.Load())
	kept, err := s.state.ReadFile(snapshotFile)
	require.NoError(t, err)
	assert.JSONEq(t, testFeed, string(kept))

	status.Store(http.StatusInternalServerError)
	served := s.auth.current.Load()
	s.feedRound(ctx)
	assert.Same(t, served, s.auth.current.Load(), "a failed read changes nothing")

	restarted, err := newService(cfg, prometheus.NewRegistry())
	require.NoError(t, err)
	defer restarted.close()
	restarted.loadSnapshot(ctx)
	z := restarted.auth.current.Load()
	require.NotNil(t, z)
	assert.Equal(t, "east", z.regions[0].name)
}

func TestService_FeedRound_NotReadyKeepsNothingNew(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	dir := t.TempDir()
	cfg := config{zone: "geo.example.com.", name: "geo.example.com.", nameservers: []string{"ns1.example.com."}, feedURL: srv.URL, stateDir: dir}

	s, err := newService(cfg, prometheus.NewRegistry())
	require.NoError(t, err)
	defer s.close()
	s.feedRound(t.Context())

	assert.Nil(t, s.auth.current.Load())
	_, err = s.state.Stat(snapshotFile)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestService_LoadSnapshot_Unusable(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, snapshotFile), []byte(`{"regions":[]}`), 0o600))
	cfg := config{zone: "geo.example.com.", name: "geo.example.com.", nameservers: []string{"ns1.example.com."}, stateDir: dir}

	s, err := newService(cfg, prometheus.NewRegistry())
	require.NoError(t, err)
	defer s.close()
	s.loadSnapshot(t.Context())

	assert.Nil(t, s.auth.current.Load())
}

func TestCanonical(t *testing.T) {
	assert.Equal(t, "geo.example.com.", canonical("Geo.Example.COM"))
	assert.Equal(t, "geo.example.com.", canonical("geo.example.com."))
	assert.Empty(t, canonical(""))
}
