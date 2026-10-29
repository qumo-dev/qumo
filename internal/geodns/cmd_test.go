package geodns

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
		"answers that are not a number": {
			env: map[string]string{"QUMO_DNS_MAX_ANSWERS": "two"}, required: true,
		},
		"a negative database interval": {
			env: map[string]string{"QUMO_DNS_GEOIP_INTERVAL": "-1h"}, required: true,
		},
		"only empty name servers": {
			env: map[string]string{"QUMO_DNS_NAMESERVERS": " , "}, required: true,
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

// feedServer serves the body in body, with 200, and counts the reads.
type feedServer struct {
	body  atomic.Pointer[string]
	reads atomic.Int32
}

func (f *feedServer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	f.reads.Add(1)
	_, _ = w.Write([]byte(*f.body.Load())) // not actionable: a test server
}

// testService is a service with a state directory, reading the feed at
// feedURL.
func testService(tb testing.TB, feedURL string) *service {
	tb.Helper()
	cfg := config{
		zone: "geo.example.com.", name: "cdn.geo.example.com.", nameservers: []string{"ns1.example.com."},
		hostmaster: "hostmaster.geo.example.com.", ttl: 30 * time.Second, maxAnswers: 2,
		feedURL: feedURL, feedInterval: time.Minute, stateDir: tb.TempDir(),
	}
	s, err := newService(cfg, prometheus.NewRegistry())
	require.NoError(tb, err)
	tb.Cleanup(s.close)
	return s
}

const testFeedTwoRegions = `{"regions":[` +
	`{"region":"east","lat":35.68,"lon":139.69,"addrs":["192.0.2.20"]},` +
	`{"region":"north","lat":43.06,"lon":141.35,"addrs":["192.0.2.10"]}],` +
	`"published_at":"2026-01-02T03:05:00Z"}`

// TestService_FeedRound_Outcomes reads a feed through its changes: the same
// body is no change and not written again, an unusable one leaves what is
// served and kept, and a new one replaces both.
func TestService_FeedRound_Outcomes(t *testing.T) {
	feed := &feedServer{}
	feed.body.Store(new(testFeed))
	srv := httptest.NewServer(feed)
	defer srv.Close()
	s := testService(t, srv.URL)
	rounds := func(outcome string) float64 {
		return testutil.ToFloat64(s.metrics.feedRounds.WithLabelValues(outcome))
	}

	s.feedRound(t.Context())
	require.NotNil(t, s.auth.current.Load())
	assert.InDelta(t, 1, rounds("updated"), 0)

	// The same body again: nothing to do, and the copy kept is not written.
	require.NoError(t, s.state.Remove(snapshotFile))
	s.feedRound(t.Context())
	assert.InDelta(t, 1, rounds("unchanged"), 0)
	_, err := s.state.Stat(snapshotFile)
	assert.ErrorIs(t, err, os.ErrNotExist, "an unchanged feed is not written again")
	require.NoError(t, writeFileAtomic(s.state, snapshotFile, []byte(testFeed)))

	// A feed with no usable region: what is served and kept stands.
	served := s.auth.current.Load()
	feed.body.Store(new(`{"regions":[]}`))
	s.feedRound(t.Context())
	assert.InDelta(t, 1, rounds("error"), 0)
	assert.Same(t, served, s.auth.current.Load())
	kept, err := s.state.ReadFile(snapshotFile)
	require.NoError(t, err)
	assert.JSONEq(t, testFeed, string(kept))

	// A new feed replaces what is served and what is kept.
	feed.body.Store(new(testFeedTwoRegions))
	s.feedRound(t.Context())
	assert.InDelta(t, 2, rounds("updated"), 0)
	assert.Len(t, s.auth.current.Load().regions, 2)
	assert.InDelta(t, 2, testutil.ToFloat64(s.metrics.zoneRegions), 0)
	assert.InDelta(t, 2, testutil.ToFloat64(s.metrics.zoneAddresses), 0)
	kept, err = s.state.ReadFile(snapshotFile)
	require.NoError(t, err)
	assert.JSONEq(t, testFeedTwoRegions, string(kept))
}

// TestService_Serve_ForgetsRegionsLeft: a region the new feed no longer has
// leaves the answer counts, and a region it keeps keeps its count.
func TestService_Serve_ForgetsRegionsLeft(t *testing.T) {
	s := testService(t, "http://127.0.0.1:1/unused")
	require.NoError(t, s.serve(t.Context(), []byte(testFeedTwoRegions), false))
	s.metrics.answers.WithLabelValues("east", "located").Inc()
	s.metrics.answers.WithLabelValues("north", "unknown").Inc()

	require.NoError(t, s.serve(t.Context(), []byte(testFeed), false))

	assert.Equal(t, 1, testutil.CollectAndCount(s.metrics.answers), "north left the feed")
	assert.InDelta(t, 1, testutil.ToFloat64(s.metrics.answers.WithLabelValues("east", "located")), 0)
}

// TestService_LoadSnapshot_ServesWithoutTheFeed: a restart with the feed
// unreachable answers from the copy kept, without asking the feed.
func TestService_LoadSnapshot_ServesWithoutTheFeed(t *testing.T) {
	s := testService(t, "http://127.0.0.1:1/unreachable")
	require.NoError(t, writeFileAtomic(s.state, snapshotFile, []byte(testFeed)))

	s.loadSnapshot(t.Context())

	resp := s.auth.answer(query("cdn.geo.example.com.", dns.TypeA), netip.MustParseAddr("192.0.2.200"))
	assert.Equal(t, dns.RcodeSuccess, resp.Rcode)
	assert.Equal(t, []string{"192.0.2.20"}, answerAddrs(t, resp))
}

func TestService_HTTPHandler(t *testing.T) {
	reg := prometheus.NewRegistry()
	s, err := newService(config{zone: "geo.example.com.", name: "geo.example.com.", nameservers: []string{"ns1.example.com."}}, reg)
	require.NoError(t, err)
	defer s.close()
	h := s.httpHandler(reg)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	assert.Equal(t, http.StatusServiceUnavailable, get("/healthz").Code, "no feed yet")
	require.NoError(t, s.serve(t.Context(), []byte(testFeed), false))
	assert.Equal(t, http.StatusOK, get("/healthz").Code)
	metrics := get("/metrics")
	assert.Equal(t, http.StatusOK, metrics.Code)
	assert.Contains(t, metrics.Body.String(), "qumo_dns_zone_regions 1")
}

// TestService_FeedLoop reads the feed at once, then on every interval, and
// stops when its context ends.
func TestService_FeedLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// A URL that cannot make a request fails each round at once, with
		// no network, which a bubble cannot wait on.
		s := testService(t, "://no-scheme")
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			s.feedLoop(ctx)
			close(done)
		}()
		errorRounds := func() float64 { return testutil.ToFloat64(s.metrics.feedRounds.WithLabelValues("error")) }

		synctest.Wait()
		assert.InDelta(t, 1, errorRounds(), 0, "read at once")
		time.Sleep(3*time.Minute + time.Second)
		synctest.Wait()
		assert.InDelta(t, 4, errorRounds(), 0, "and every minute")

		cancel()
		<-done
	})
}

// TestService_GeoLoop checks for a newer database at once, then on every
// interval, and stops when its context ends.
func TestService_GeoLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := testService(t, "://no-scheme")
		s.cfg.geoURL = "://no-scheme"
		s.cfg.geoDB = filepath.Join(s.cfg.stateDir, geoFile)
		s.cfg.geoInterval = time.Hour
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			s.geoLoop(ctx)
			close(done)
		}()
		errorRounds := func() float64 { return testutil.ToFloat64(s.metrics.geoRounds.WithLabelValues("error")) }

		synctest.Wait()
		assert.InDelta(t, 1, errorRounds(), 0, "checked at once")
		time.Sleep(2*time.Hour + time.Second)
		synctest.Wait()
		assert.InDelta(t, 3, errorRounds(), 0, "and every hour")

		cancel()
		<-done
	})
}

// TestService_GeoLoop_LoadsTheDownload puts a downloaded database in place,
// so the next query is placed by it.
func TestService_GeoLoop_LoadsTheDownload(t *testing.T) {
	db := testCityDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(db) // not actionable: a test server
	}))
	defer srv.Close()
	s := testService(t, srv.URL)
	s.cfg.geoURL = srv.URL + "/city.mmdb"
	s.cfg.geoDB = filepath.Join(s.cfg.stateDir, geoFile)
	s.cfg.geoInterval = time.Hour
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		s.geoLoop(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	require.Eventually(t, func() bool {
		_, ok := s.geo.locate(netip.MustParseAddr("198.51.100.7"))
		return ok
	}, 5*time.Second, 10*time.Millisecond)
	assert.InDelta(t, 1, testutil.ToFloat64(s.metrics.geoRounds.WithLabelValues("updated")), 0)
}

func TestRun_AMissingDatabaseWithNoURLFails(t *testing.T) {
	cfg := config{
		zone: "geo.example.com.", name: "geo.example.com.", nameservers: []string{"ns1.example.com."},
		geoDB: filepath.Join(t.TempDir(), "missing.mmdb"),
	}

	err := run(t.Context(), cfg)

	assert.ErrorContains(t, err, "QUMO_DNS_GEOIP_DB")
}

func TestConfigFromEnv_Settings(t *testing.T) {
	setRequired(t)
	t.Setenv("QUMO_DNS_NAME", "CDN.geo.example.com")
	t.Setenv("QUMO_DNS_NAMESERVERS", "ns1.example.com,, ns2.example.com ,")
	t.Setenv("QUMO_DNS_HTTP_ADDR", "off")
	t.Setenv("QUMO_DNS_MAX_ANSWERS", "4")
	t.Setenv("QUMO_DNS_TTL", "1m")
	t.Setenv("QUMO_DNS_GEOIP_DB", "/data/city.mmdb")
	t.Setenv("QUMO_DNS_GEOIP_URL", "https://db.example/city.mmdb")

	cfg, err := configFromEnv()

	require.NoError(t, err)
	assert.Equal(t, "cdn.geo.example.com.", cfg.name)
	assert.Equal(t, []string{"ns1.example.com.", "ns2.example.com."}, cfg.nameservers, "empty entries are dropped")
	assert.Equal(t, "off", cfg.httpAddr)
	assert.Equal(t, 4, cfg.maxAnswers)
	assert.Equal(t, time.Minute, cfg.ttl)
	assert.Equal(t, "/data/city.mmdb", cfg.geoDB, "a database named is where the download goes")
}

// TestConfigFromEnv_NamesEveryMissingSetting: one error names them all, so
// that an operator fixes the environment once.
func TestConfigFromEnv_NamesEveryMissingSetting(t *testing.T) {
	for _, key := range []string{"QUMO_DNS_ZONE", "QUMO_DNS_FEED_URL", "QUMO_DNS_NAMESERVERS"} {
		t.Setenv(key, "")
	}

	_, err := configFromEnv()

	require.Error(t, err)
	for _, key := range []string{"QUMO_DNS_ZONE", "QUMO_DNS_FEED_URL", "QUMO_DNS_NAMESERVERS"} {
		assert.ErrorContains(t, err, key)
	}
}

func TestCanonical(t *testing.T) {
	assert.Equal(t, "geo.example.com.", canonical("Geo.Example.COM"))
	assert.Equal(t, "geo.example.com.", canonical("geo.example.com."))
	assert.Empty(t, canonical(""))
}
