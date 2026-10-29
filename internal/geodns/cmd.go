// Package geodns is `qumo dns`: an authoritative DNS server that answers one
// name with the addresses of the region nearest the client.
//
// It holds no inventory of its own. A feed, read over HTTPS, says which
// regions exist, where they are, and their addresses; the server answers
// from its last good reading, keeps that reading on disk, and goes on
// answering from it while the feed cannot be read. A client is placed with a
// city database in MaxMind's format, by the subnet a resolver sends (EDNS
// Client Subnet) or the resolver's own address; one that cannot be placed
// gets every region in turn.
//
// It is meant to run beside each relay, so that every place that serves
// clients also answers where to connect. It does not recurse: names outside
// its zone are refused.
package geodns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/qumo-dev/qumo/internal/envconfig"
)

const (
	defaultListen       = ":53"
	defaultHTTPAddr     = "127.0.0.1:9153"
	defaultTTL          = 30 * time.Second
	defaultMaxAnswers   = 2
	defaultFeedInterval = 15 * time.Second
	defaultGeoInterval  = 24 * time.Hour
	feedTimeout         = 10 * time.Second
	geoTimeout          = 10 * time.Minute
	shutdownTimeout     = 5 * time.Second
	// snapshotFile, in the state directory, is the last good feed.
	snapshotFile = "feed.json"
	// geoFile, in the state directory, is the downloaded location database.
	geoFile = "city.mmdb"
)

// config is what Run reads from the environment.
type config struct {
	listen       string
	httpAddr     string
	zone         string
	name         string
	nameservers  []string
	hostmaster   string
	ttl          time.Duration
	maxAnswers   int
	feedURL      string
	feedToken    string
	feedInterval time.Duration
	stateDir     string
	geoURL       string
	geoDB        string
	geoInterval  time.Duration
}

// Run starts the DNS server. Configuration is read from environment
// variables:
//
//	QUMO_DNS_ZONE           - the zone this server is authoritative for, e.g. "geo.example.com" (required)
//	QUMO_DNS_NAME           - the name answered with addresses, in the zone (default: the zone itself)
//	QUMO_DNS_NAMESERVERS    - the zone's name servers, comma-separated host names, as delegated (required)
//	QUMO_DNS_FEED_URL       - where the regions and their addresses are read (required; the feed in docs/site/content/en/docs/cli/dns.md)
//	QUMO_DNS_FEED_TOKEN     - bearer token sent to the feed (default: none)
//	QUMO_DNS_FEED_INTERVAL  - how often the feed is read (default: "15s")
//	QUMO_DNS_STATE_DIR      - a directory for the last good feed and a downloaded database (default: none, nothing is kept)
//	QUMO_DNS_GEOIP_DB       - a city database (.mmdb) to place clients with (default: <state dir>/city.mmdb with a URL, else none)
//	QUMO_DNS_GEOIP_URL      - where to download that database; "{month}" is replaced with the month, "2006-01" (default: none)
//	QUMO_DNS_GEOIP_INTERVAL - how often the download is checked for a newer file (default: "24h")
//	QUMO_DNS_LISTEN         - UDP and TCP listen address (default: ":53")
//	QUMO_DNS_HTTP_ADDR      - HTTP address of /healthz and /metrics (default: "127.0.0.1:9153"; "off" for none)
//	QUMO_DNS_TTL            - TTL of the address answers (default: "30s")
//	QUMO_DNS_MAX_ANSWERS    - addresses in one answer, all of the nearest region (default: 2)
//	QUMO_DNS_HOSTMASTER     - the SOA contact, as a name (default: "hostmaster.<zone>")
//
// Without a database every client is of unknown place, and the regions are
// answered in turn.
func Run(_ []string) error {
	cfg, err := configFromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return run(ctx, cfg)
}

func configFromEnv() (config, error) {
	var missing []string
	required := func(key string) string {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			missing = append(missing, key)
		}
		return v
	}
	cfg := config{
		listen:    envconfig.String("QUMO_DNS_LISTEN", defaultListen),
		httpAddr:  envconfig.String("QUMO_DNS_HTTP_ADDR", defaultHTTPAddr),
		zone:      canonical(required("QUMO_DNS_ZONE")),
		feedURL:   required("QUMO_DNS_FEED_URL"),
		feedToken: os.Getenv("QUMO_DNS_FEED_TOKEN"),
		stateDir:  os.Getenv("QUMO_DNS_STATE_DIR"),
		geoURL:    os.Getenv("QUMO_DNS_GEOIP_URL"),
		geoDB:     os.Getenv("QUMO_DNS_GEOIP_DB"),
	}
	for ns := range strings.SplitSeq(os.Getenv("QUMO_DNS_NAMESERVERS"), ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			cfg.nameservers = append(cfg.nameservers, canonical(ns))
		}
	}
	if len(cfg.nameservers) == 0 {
		// The SOA names the first: a list of commas is no list.
		missing = append(missing, "QUMO_DNS_NAMESERVERS")
	}
	if len(missing) > 0 {
		return config{}, fmt.Errorf("dns: missing required settings: %s", strings.Join(missing, ", "))
	}
	cfg.name = canonical(envconfig.String("QUMO_DNS_NAME", cfg.zone))
	cfg.hostmaster = canonical(envconfig.String("QUMO_DNS_HOSTMASTER", "hostmaster."+cfg.zone))

	var errs []error
	durations := []struct {
		key string
		def time.Duration
		to  *time.Duration
	}{
		{"QUMO_DNS_TTL", defaultTTL, &cfg.ttl},
		{"QUMO_DNS_FEED_INTERVAL", defaultFeedInterval, &cfg.feedInterval},
		{"QUMO_DNS_GEOIP_INTERVAL", defaultGeoInterval, &cfg.geoInterval},
	}
	for _, d := range durations {
		v, set, err := envconfig.Duration(d.key)
		switch {
		case err != nil:
			errs = append(errs, err)
		case !set:
			*d.to = d.def
		case v <= 0:
			errs = append(errs, fmt.Errorf("invalid %s %q: want a duration above zero", d.key, os.Getenv(d.key)))
		default:
			*d.to = v
		}
	}
	cfg.maxAnswers = defaultMaxAnswers
	if raw := strings.TrimSpace(os.Getenv("QUMO_DNS_MAX_ANSWERS")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			errs = append(errs, fmt.Errorf("invalid QUMO_DNS_MAX_ANSWERS %q: want a whole number, one or more", raw))
		}
		cfg.maxAnswers = n
	}
	if !dns.IsSubDomain(cfg.zone, cfg.name) {
		errs = append(errs, fmt.Errorf("QUMO_DNS_NAME %q is not in QUMO_DNS_ZONE %q", cfg.name, cfg.zone))
	}
	if cfg.geoURL != "" && cfg.geoDB == "" {
		if cfg.stateDir == "" {
			errs = append(errs, errors.New("QUMO_DNS_GEOIP_URL needs QUMO_DNS_STATE_DIR or QUMO_DNS_GEOIP_DB: somewhere to keep the database"))
		}
		cfg.geoDB = filepath.Join(cfg.stateDir, geoFile)
	}
	if len(errs) > 0 {
		return config{}, fmt.Errorf("dns: %w", errors.Join(errs...))
	}
	return cfg, nil
}

// canonical is a name as compared and answered: lower case, fully qualified.
func canonical(name string) string {
	if name == "" {
		return ""
	}
	return strings.ToLower(dns.Fqdn(name))
}

// service is one running server: the authority and what keeps it fed.
type service struct {
	cfg     config
	state   *os.Root // the state directory, or nil for none
	auth    *authority
	feed    *feedClient
	geo     *locator
	metrics *metrics
	// lastBody is the feed body last served, to tell a change from none.
	lastBody []byte
}

// newService builds the server from cfg. Its state directory, when cfg names
// one, is opened (and made) here; close releases it.
func newService(cfg config, reg prometheus.Registerer) (*service, error) {
	var state *os.Root
	if cfg.stateDir != "" {
		if err := os.MkdirAll(cfg.stateDir, 0o700); err != nil {
			return nil, fmt.Errorf("dns: QUMO_DNS_STATE_DIR: %w", err)
		}
		root, err := os.OpenRoot(cfg.stateDir)
		if err != nil {
			return nil, fmt.Errorf("dns: QUMO_DNS_STATE_DIR: %w", err)
		}
		state = root
	}
	m := newMetrics(reg)
	geo := &locator{}
	return &service{
		cfg:     cfg,
		state:   state,
		geo:     geo,
		metrics: m,
		feed:    &feedClient{url: cfg.feedURL, token: cfg.feedToken, client: &http.Client{Timeout: feedTimeout}},
		auth: &authority{
			zone:        cfg.zone,
			name:        cfg.name,
			nameservers: cfg.nameservers,
			hostmaster:  cfg.hostmaster,
			ttl:         uint32(cfg.ttl / time.Second),
			maxAnswers:  cfg.maxAnswers,
			current:     &atomic.Pointer[zone]{},
			geo:         geo,
			metrics:     m,
		},
	}, nil
}

// close releases what the service holds.
func (s *service) close() {
	s.geo.close()
	if s.state != nil {
		_ = s.state.Close() // not actionable: the process is ending
	}
}

func run(ctx context.Context, cfg config) error {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	s, err := newService(cfg, reg)
	if err != nil {
		return err
	}
	defer s.close()

	s.loadSnapshot(ctx)
	if cfg.geoDB != "" {
		if err := s.geo.load(cfg.geoDB); err != nil && cfg.geoURL == "" {
			return fmt.Errorf("dns: QUMO_DNS_GEOIP_DB: %w", err)
		}
	}
	slog.Info("dns: starting",
		"zone", cfg.zone, "name", cfg.name, "listen", cfg.listen,
		"nameservers", strings.Join(cfg.nameservers, ","),
		"geoip", cfg.geoDB != "", "snapshot", cfg.stateDir != "")

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return serveDNS(ctx, &dns.Server{Addr: cfg.listen, Net: "udp", Handler: s.auth}) })
	g.Go(func() error { return serveDNS(ctx, &dns.Server{Addr: cfg.listen, Net: "tcp", Handler: s.auth}) })
	g.Go(func() error {
		s.feedLoop(ctx)
		return nil
	})
	if cfg.geoURL != "" {
		g.Go(func() error {
			s.geoLoop(ctx)
			return nil
		})
	}
	if cfg.httpAddr != "off" {
		g.Go(func() error { return s.serveHTTP(ctx, reg) })
	}
	return g.Wait()
}

// serveDNS runs srv until ctx ends.
func serveDNS(ctx context.Context, srv *dns.Server) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return fmt.Errorf("dns: serve %s on %s: %w", srv.Net, srv.Addr, err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if err := srv.ShutdownContext(shutdownCtx); err != nil {
			slog.Warn("dns: shut down", "net", srv.Net, "err", err)
		}
		<-errc
		return nil
	}
}

// httpHandler serves /healthz, 200 once a feed is served, and /metrics.
func (s *service) httpHandler(reg *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if s.auth.current.Load() == nil {
			http.Error(w, "no feed has been read", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n")) // not actionable: the client has gone
	})
	return mux
}

// serveHTTP serves httpHandler until ctx ends.
func (s *service) serveHTTP(ctx context.Context, reg *prometheus.Registry) error {
	srv := &http.Server{Addr: s.cfg.httpAddr, Handler: s.httpHandler(reg), ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", s.cfg.httpAddr)
	if err != nil {
		return fmt.Errorf("dns: listen on %s: %w", s.cfg.httpAddr, err)
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return fmt.Errorf("dns: serve http: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx) // not actionable: the process is ending
		<-errc
		return nil
	}
}

// loadSnapshot serves the feed kept on disk, so that a restart while the
// feed cannot be read still answers.
func (s *service) loadSnapshot(ctx context.Context) {
	if s.state == nil {
		return
	}
	body, err := s.state.ReadFile(snapshotFile)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		slog.WarnContext(ctx, "dns: read the kept feed", "dir", s.cfg.stateDir, "err", err)
		return
	}
	if err := s.serve(ctx, body, false); err != nil {
		slog.WarnContext(ctx, "dns: the kept feed is not usable", "dir", s.cfg.stateDir, "err", err)
		return
	}
	slog.InfoContext(ctx, "dns: answering from the kept feed until the feed is read", "dir", s.cfg.stateDir)
}

// feedLoop reads the feed now and on every interval until ctx ends.
func (s *service) feedLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.feedInterval)
	defer ticker.Stop()
	for {
		s.feedRound(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// feedRound reads the feed once. Whatever fails, what is served stays.
func (s *service) feedRound(ctx context.Context) {
	body, err := s.feed.fetch(ctx)
	switch {
	case errors.Is(err, errFeedNotReady):
		s.metrics.feedRounds.WithLabelValues("not_ready").Inc()
		slog.InfoContext(ctx, "dns: the feed has nothing yet; what is served stands")
		return
	case err != nil:
		s.metrics.feedRounds.WithLabelValues("error").Inc()
		if ctx.Err() == nil {
			slog.WarnContext(ctx, "dns: the feed could not be read; what is served stands", "err", err)
		}
		return
	}
	s.metrics.feedUpdated.SetToCurrentTime()
	if string(body) == string(s.lastBody) {
		s.metrics.feedRounds.WithLabelValues("unchanged").Inc()
		return
	}
	if err := s.serve(ctx, body, true); err != nil {
		s.metrics.feedRounds.WithLabelValues("error").Inc()
		slog.WarnContext(ctx, "dns: the feed is not usable; what is served stands", "err", err)
		return
	}
	s.metrics.feedRounds.WithLabelValues("updated").Inc()
}

// forgetRegionsLeft drops the answer counts of the regions in old that are
// not in z, so that a feed renaming its regions does not leave every name it
// ever had in the metrics. A query answered from old as it is replaced may
// count its region once more.
func (s *service) forgetRegionsLeft(old, z *zone) {
	if old == nil {
		return
	}
	for _, r := range old.regions {
		if !slices.ContainsFunc(z.regions, func(n region) bool { return n.name == r.name }) {
			s.metrics.answers.DeletePartialMatch(prometheus.Labels{"region": r.name})
		}
	}
}

// serve puts a feed body in place, and keeps it on disk when keep is set.
func (s *service) serve(ctx context.Context, body []byte, keep bool) error {
	z, skipped, err := parseFeed(body)
	for _, why := range skipped {
		slog.WarnContext(ctx, "dns: a feed entry was left out", "why", why)
	}
	if err != nil {
		return err
	}
	old := s.auth.current.Swap(z)
	s.lastBody = body
	s.forgetRegionsLeft(old, z)
	addrs := 0
	for _, r := range z.regions {
		addrs += len(r.addrs)
	}
	s.metrics.zoneRegions.Set(float64(len(z.regions)))
	s.metrics.zoneAddresses.Set(float64(addrs))
	slog.InfoContext(ctx, "dns: serving the feed", "regions", len(z.regions), "addresses", addrs, "serial", z.serial)
	if keep && s.state != nil {
		if err := writeFileAtomic(s.state, snapshotFile, body); err != nil {
			slog.WarnContext(ctx, "dns: keep the feed on disk", "err", err)
		}
	}
	return nil
}

// geoLoop keeps the location database fresh until ctx ends.
func (s *service) geoLoop(ctx context.Context) {
	fetcher := &geoFetcher{urlTemplate: s.cfg.geoURL, dest: s.cfg.geoDB, client: &http.Client{Timeout: geoTimeout}, now: time.Now}
	ticker := time.NewTicker(s.cfg.geoInterval)
	defer ticker.Stop()
	for {
		err := fetcher.refresh(ctx)
		switch {
		case errors.Is(err, errNotModified):
			s.metrics.geoRounds.WithLabelValues("unchanged").Inc()
		case err != nil:
			s.metrics.geoRounds.WithLabelValues("error").Inc()
			if ctx.Err() == nil {
				slog.WarnContext(ctx, "dns: the location database could not be refreshed; the one held stands", "err", err)
			}
		default:
			if err := s.geo.load(s.cfg.geoDB); err != nil {
				s.metrics.geoRounds.WithLabelValues("error").Inc()
				slog.WarnContext(ctx, "dns: load the refreshed location database", "err", err)
			} else {
				s.metrics.geoRounds.WithLabelValues("updated").Inc()
				slog.InfoContext(ctx, "dns: location database refreshed", "path", s.cfg.geoDB)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
