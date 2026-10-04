// Package loadgen provides two out-of-process capacity primitives that connect
// to a running qumo relay:
//
//	loadgen publish        — one trickle publisher (a small group every 1/GPS s)
//	loadgen subscribe <N>  — launch N subscriber sessions and measure the hold
//
// Both are pure remote clients: they dial the relay you point them at (--relay
// + --ca, or --insecure for a self-signed dev relay) and never spawn a relay or
// a cert of their own. Orchestration —
// sweeping a list of session counts, or climbing to find the ceiling — lives in
// a separate driver (tools/capacity) that composes these primitives, keeping
// the CLI itself small.
//
// The point of measuring out-of-process is fidelity: with the relay in its own
// process (ideally its own host), `subscribe` reports the RELAY's own
// per-session cost by scraping its /metrics endpoint (go_goroutines,
// process_resident_memory_bytes, qumo_relay_sessions_active) before and after
// the run — the number reflects the relay, not the load generator. With
// --results it also appends a capacity-group JSONL record in the schema the
// relay-bench dashboard reads.
package loadgen

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/relay"
	"github.com/qumo-dev/qumo/internal/tlsclient"
)

// Run dispatches a loadgen subcommand. It is the package entrypoint wired into
// the top-level `qumo loadgen` command.
func Run(args []string) error {
	if len(args) < 1 {
		usage(os.Stderr)
		return errors.New("loadgen: a subcommand is required (subscribe|publish)")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "subscribe":
		return runSubscribe(rest)
	case "publish":
		return runPublish(rest)
	case "-h", "--help", "help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("loadgen: unknown subcommand %q", sub)
	}
}

func usage(w io.Writer) {
	_, _ = io.WriteString(w, `Usage: qumo loadgen <subcommand> [flags]

Out-of-process capacity primitives against a running qumo relay. Both are pure
remote clients — they dial --relay (trusting --ca, or --insecure for a self-signed dev relay) and never spawn a relay.

Subcommands:
  publish          Publish one trickle track to the relay (keep running during a run)
  subscribe <N>    Launch N subscriber sessions and measure the relay's hold + per-session cost

To sweep a list of counts or auto-find the ceiling, use the driver: tools/capacity.

Run "qumo loadgen <subcommand> -h" for that subcommand's flags.
`)
}

// ---- shared dial config ----

// dialTarget holds the connection parameters shared by both subcommands.
type dialTarget struct {
	relay   string // the relay's host:port, safe to log
	url     string // moqt:// URL dialed; may carry a credential, so never logged
	metrics string // relay /metrics URL (subscribe only)
	path    string
	track   string
	tls     *tls.Config
	quic    *quic.Config
}

// newTarget builds a dialTarget. The relay's certificate is verified against a
// PEM trust anchor (caFile) unless insecure is set, in which case verification
// is skipped for a self-signed dev relay. An empty metrics URL defaults to
// http://<relay>/metrics. The trust decision is shared across qumo's clients
// via [tlsclient.Apply]; only the load generator's base config (MoQ ALPN, TLS
// 1.3 floor) and the QUIC tuning are owned here.
//
// relay is the relay's host:port, or a moqt:// URL whose query may carry a
// credential (?jwt=…) for a relay with an auth server. Only the host:port is
// logged.
func newTarget(relay, metrics, path, track, caFile string, insecure bool, idle, keepalive time.Duration) (dialTarget, error) {
	u, err := parseRelay(relay)
	if err != nil {
		return dialTarget{}, fmt.Errorf("--relay: %w", err)
	}
	m := metrics
	if m == "" {
		m = "http://" + u.Host + "/metrics"
	}
	tc := &tls.Config{
		NextProtos: []string{moqt.NextProtoMOQ},
		MinVersion: tls.VersionTLS13,
	}
	if err := tlsclient.Apply(tc, caFile, insecure); err != nil {
		return dialTarget{}, err
	}
	return dialTarget{
		relay:   u.Host,
		url:     u.String(),
		metrics: m,
		path:    path,
		track:   track,
		tls:     tc,
		quic: &quic.Config{
			EnableDatagrams: true,
			KeepAlivePeriod: keepalive,
			MaxIdleTimeout:  idle,
		},
	}, nil
}

// parseRelay parses --relay: a host:port, or a moqt:// URL whose query may
// carry a credential.
func parseRelay(relay string) (*url.URL, error) {
	if !strings.Contains(relay, "://") {
		relay = "moqt://" + relay
	}
	u, err := url.Parse(relay)
	if err != nil {
		// url.Parse's *url.Error quotes the whole URL, credential and all;
		// report only the reason it wraps.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "moqt" {
		return nil, fmt.Errorf("want host:port or a moqt:// URL, got scheme %q", u.Scheme)
	}
	return u, nil
}

// bindCommon registers the flags common to publish/subscribe on fs and returns a
// finalizer that validates them and builds the TLS/QUIC config after Parse.
func bindCommon(fs *flag.FlagSet) func() (dialTarget, error) {
	relay := fs.String("relay", "127.0.0.1:4433", "relay moqt address (host:port), or a moqt:// URL carrying a credential (moqt://host:port/path?jwt=…)")
	metrics := fs.String("metrics", "", "relay /metrics URL (default http://<relay>/metrics)")
	caFile := fs.String("ca", "", "PEM file of the relay's TLS cert/CA to trust (required unless --insecure)")
	path := fs.String("path", "/bench/carry", "broadcast path")
	track := fs.String("track", "data", "track name")
	idle := fs.Duration("idle-timeout", 30*time.Second, "QUIC max idle timeout")
	keepalive := fs.Duration("keepalive", 5*time.Second, "QUIC keep-alive period")
	insecure := fs.Bool("insecure", false, "skip relay TLS verification (dev; self-signed relay)")
	return func() (dialTarget, error) {
		if *caFile == "" && !*insecure {
			return dialTarget{}, errors.New("--ca is required (or --insecure for a self-signed dev relay)")
		}
		return newTarget(*relay, *metrics, *path, *track, *caFile, *insecure, *idle, *keepalive)
	}
}

func dialer(t dialTarget) *moqt.Dialer {
	return &moqt.Dialer{TLSConfig: t.tls, QUICConfig: t.quic}
}

// ---- publish ----

func runPublish(args []string) error {
	fs := flag.NewFlagSet("loadgen publish", flag.ContinueOnError)
	finalize := bindCommon(fs)
	gps := fs.Float64("gps", 0.5, "groups per second (trickle rate)")
	size := fs.Int("size", 64, "frame size in bytes (min 16)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	target, err := finalize()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	slog.Info("loadgen publishing", "relay", target.relay, "path", target.path, "gps", *gps, "size", *size)
	return publishTrickle(ctx, target, *gps, *size)
}

// publishTrickle publishes one track that emits a small group every 1/gps s,
// blocking until ctx is cancelled. Shared by `publish` and `sweep`.
func publishTrickle(ctx context.Context, target dialTarget, gps float64, size int) error {
	if size < 16 {
		size = 16
	}
	interval := time.Duration(float64(time.Second) / gps)
	mux := moqt.NewTrackMux(moqt.NewHopID())
	mux.PublishFunc(ctx, moqt.BroadcastPath(target.path), func(tw *moqt.TrackWriter) {
		defer tw.Close()
		payload := make([]byte, size)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				binary.BigEndian.PutUint64(payload[8:16], uint64(time.Now().UnixNano()))
				gw, err := tw.OpenGroup(ctx)
				if err != nil || gw == nil {
					continue
				}
				fr := moqt.NewFrame(size)
				_, _ = fr.Write(payload)
				_ = gw.WriteFrame(fr)
				_ = gw.Close()
			}
		}
	})
	sess, err := dialer(target).Dial(ctx, target.url, mux)
	if err != nil {
		return fmt.Errorf("dial relay %s: %w", target.relay, err)
	}
	defer sess.CloseWithError(moqt.NoError, "done")
	<-ctx.Done()
	return nil
}

// ---- subscribe ----

func runSubscribe(args []string) error {
	fs := flag.NewFlagSet("loadgen subscribe", flag.ContinueOnError)
	finalize := bindCommon(fs)
	hold := fs.Duration("hold", 30*time.Second, "how long to hold sessions after establishment")
	ramp := fs.Float64("ramp", 0, "admission pacing in sessions/second (0 = launch all at once)")
	resultsDir := fs.String("results", "", "dir to append a capacity JSONL record (optional)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// N is a positional arg (flags precede it): `loadgen subscribe <flags> N`.
	if fs.NArg() != 1 {
		return errors.New("usage: loadgen subscribe [flags] <N>  (N = number of subscriber sessions)")
	}
	if *ramp < 0 {
		return fmt.Errorf("--ramp must be >= 0 (sessions/second admission pacing), got %v", *ramp)
	}
	sessions, err := strconv.Atoi(fs.Arg(0))
	if err != nil || sessions < 1 {
		return fmt.Errorf("invalid session count %q (want a positive integer)", fs.Arg(0))
	}
	target, err := finalize()
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	res, err := runCarry(ctx, target, sessions, *hold, *ramp)
	if err != nil {
		return err
	}
	report(target, sessions, res)
	if *resultsDir != "" {
		if err := emitJSONL(*resultsDir, sessions, res); err != nil {
			slog.Warn("loadgen results emission failed", "err", err)
		}
	}
	return nil
}

// carryResult is the measured outcome of a subscribe run. All resource deltas
// are the RELAY's (scraped from /metrics), not the load generator's.
type carryResult struct {
	connected       int
	receiving       int
	relayGoros      int     // go_goroutines delta on the relay
	relayRSSMB      float64 // process_resident_memory_bytes delta on the relay, MB
	relaySessions   float64 // qumo_relay_sessions_active at steady state
	metricsScraped  bool    // false if the relay /metrics could not be read
	perSessionKB    float64 // relayRSSMB*1024 / connected
	perSessionGoros float64 // relayGoros / connected
	latSamples      int64   // end-to-end latency samples observed
	latP50          time.Duration
	latP95          time.Duration
	latP99          time.Duration
	rampPerSec      float64 // admission pacing used (0 = burst), reported for provenance
	launched        int     // sessions actually launched (ramp aborts mid-way launch fewer)
}

func runCarry(ctx context.Context, target dialTarget, sessions int, hold time.Duration, rampPerSec float64) (carryResult, error) {
	client := &http.Client{Timeout: 5 * time.Second}

	// Let the relay's RSS settle before taking the baseline snapshot. Without
	// this pause the base scrape may catch transient startup allocations (TLS
	// setup, initial memory pools, listener creation) that the Go GC collects
	// before the steady-state scrape. For small session counts the subscriber
	// RSS delta can then be smaller than the GC'd startup memory, producing a
	// negative heap_mb in the dashboard. A one-second pause is enough for the
	// young-gen GC to run and release those transient allocations.
	// Inline context-aware sleep: the function was previously in sweep.go (deleted).
	select {
	case <-ctx.Done():
		return carryResult{}, ctx.Err()
	case <-time.After(time.Second):
	}
	base, baseErr := fetchMetrics(ctx, client, target.metrics)
	if baseErr != nil {
		slog.Warn("relay /metrics unreadable; per-session cost will be unavailable", "url", target.metrics, "err", baseErr)
	}

	// Subscribers live under subCtx: they hold until we cancel it (right after
	// the steady-state scrape), then return and record their result. A generous
	// safety deadline guarantees they can never outlive the run.
	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()
	safety := hold + 60*time.Second

	// connCount is both the connected tally and the drain signal (subscribeOne
	// increments it on a successful subscribe); receivingCount tallies sessions
	// that got >=1 frame. Both are atomics rather than shared slices read after
	// a timed drain, so the post-drain read is race-free even if a straggler
	// subscriber is still exiting past the drain deadline.
	var connCount atomic.Int64
	var receivingCount atomic.Int64
	var wg sync.WaitGroup
	hist := &latencyHist{}

	// Launch the sessions. With ramp 0 (the default) they go out as one burst:
	// dialWithRetry's exponential backoff then spreads only the *re*dials so a
	// failed wave does not re-dial in lockstep. With --ramp R the FIRST dials are
	// paced at R sessions/second, trading a longer admission window for not
	// measuring handshake-burst capacity when the probe targets the steady-state
	// holding regime (a burst caps on simultaneous handshakes long before the
	// relay's holding ceiling — see ledger C23/C24).
	launched := launchSubscribers(subCtx, &wg, sessions, rampPerSec, func() {
		if subscribeOne(subCtx, target, safety, &connCount, hist) > 0 {
			receivingCount.Add(1)
		}
	})
	if launched < sessions {
		slog.Warn("admission cancelled mid-ramp; verdict computed over launched sessions only", "launched", launched, "offered", sessions)
	}

	// Hold: wait for sessions to connect (backoff handles retries), then hold
	// at steady state before the second scrape. A ramped admission needs its
	// full pacing window before the 30 s settle deadline starts to bite.
	admitFor := time.Duration(0)
	if rampPerSec > 0 {
		admitFor = time.Duration(float64(sessions) / rampPerSec * float64(time.Second))
	}
	settleFor(ctx, &connCount, launched, 30*time.Second+admitFor)
	// Discard ramp-phase latency samples so the reported percentiles + frame
	// count reflect only the steady-state hold window (frames/hold = true fps).
	hist.reset()
	select {
	case <-ctx.Done():
	case <-time.After(hold):
	}

	// Scrape while the sessions are still established, THEN release the
	// subscribers so their per-session results (connected/frames) are recorded.
	steady, steadyErr := fetchMetrics(ctx, client, target.metrics)
	subCancel()
	drain(&wg, 20*time.Second)

	res := carryResult{
		connected:  int(connCount.Load()),
		receiving:  int(receivingCount.Load()),
		latSamples: hist.samples(),
		latP50:     hist.percentile(50),
		latP95:     hist.percentile(95),
		latP99:     hist.percentile(99),
		rampPerSec: rampPerSec,
		launched:   launched,
	}
	if baseErr == nil && steadyErr == nil {
		res.metricsScraped = true
		// Clamp at zero: RSS/goroutines can never decrease from adding sessions.
		// A negative delta is a measurement artifact (startup transients at base
		// that were GC'd by the time steady was taken).
		res.relayGoros = max(0, int(steady.goroutines-base.goroutines))
		deltaRSS := steady.rssBytes - base.rssBytes
		if deltaRSS < 0 {
			deltaRSS = 0
		}
		res.relayRSSMB = deltaRSS / (1024 * 1024)
		res.relaySessions = steady.sessionsActive
		if res.connected > 0 {
			res.perSessionKB = res.relayRSSMB * 1024 / float64(res.connected)
			res.perSessionGoros = float64(res.relayGoros) / float64(res.connected)
		}
	}

	return res, nil
}

// launchSubscribers launches all n sessions, pacing the first dials at
// rampPerSec sessions/second when ramp > 0 (0 = all at once, the historical
// burst). Each session runs fn in its own goroutine tracked by wg. Returns the
// number of sessions actually launched; launches stop early if ctx is
// cancelled, so a mid-ramp abort never leaves the wait un-bounded.
func launchSubscribers(ctx context.Context, wg *sync.WaitGroup, n int, rampPerSec float64, fn func()) int {
	start := time.Now()
	launched := 0
	for i := range n {
		if rampPerSec > 0 && i > 0 {
			next := start.Add(time.Duration(float64(i) / rampPerSec * float64(time.Second)))
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return launched
			case <-timer.C:
			}
		}
		launched++
		wg.Go(fn)
	}
	return launched
}

// subscribeOne dials one session, subscribes, and counts received frames until
// the deadline or cancellation. It increments connCount once on a successful
// subscribe (so connCount is the "connected" tally) and returns the frame count
// (0 if it never subscribed).
//
// Dial failures are retried with exponential backoff + jitter so that a burst
// of simultaneous subscriber connections (common in fan-out benchmarks) does
// not trigger a thundering-herd of synchronized re-dials that overwhelms the
// relay's handshake capacity.
func subscribeOne(ctx context.Context, target dialTarget, dur time.Duration, connCount *atomic.Int64, hist *latencyHist) int {
	dctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()
	sess, err := dialWithRetry(dctx, target)
	if err != nil {
		return 0
	}
	defer sess.CloseWithError(moqt.NoError, "done")
	tr, err := sess.Subscribe(dctx, moqt.BroadcastPath(target.path), moqt.TrackName(target.track), nil)
	if err != nil {
		return 0
	}
	connCount.Add(1)
	defer tr.Close()
	buf := moqt.NewFrame(1500)
	n := 0
	for {
		gr, err := tr.AcceptGroup(dctx)
		if err != nil {
			break
		}
		for frame := range gr.Frames(buf) {
			n++
			// End-to-end latency: the publisher writes UnixNano at payload[8:16]
			// (publishTrickle). Both ends share a clock on a single host, so
			// now - that stamp is the frame's hub→edge→subscriber latency.
			if hist != nil {
				if body := frame.Body(); len(body) >= 16 {
					if ns := int64(binary.BigEndian.Uint64(body[8:16])); ns > 0 {
						hist.observe(time.Since(time.Unix(0, ns)))
					}
				}
			}
		}
	}
	return n
}

// dialWithRetry dials the relay with exponential backoff + jitter on failure.
// It retries until the dial succeeds or ctx is cancelled, using the relay
// package's DialBackoff to spread out synchronized re-dials across subscriber
// goroutines with the same algorithm the server-side maintainPeer uses.
func dialWithRetry(ctx context.Context, target dialTarget) (*moqt.Session, error) {
	var backoff = relay.DialBackoff{Base: 1 * time.Second, Max: 30 * time.Second}
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		sess, err := dialer(target).Dial(ctx, target.url, moqt.NewTrackMux(0))
		if err == nil {
			return sess, nil
		}
		slog.Debug("loadgen dial failed, retrying", "relay", target.relay, "retry_attempt", backoff.Attempts()+1, "err", err)
		if !backoff.Wait(ctx) {
			return nil, ctx.Err()
		}
	}
}

// settleFor waits until connCount reaches want or the deadline elapses.
func settleFor(ctx context.Context, connCount *atomic.Int64, want int, deadline time.Duration) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(deadline)
	for {
		if int(connCount.Load()) >= want {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-timeout:
			return
		case <-tick.C:
		}
	}
}

// drain waits for all subscriber goroutines to finish, bounded by timeout so a
// stuck subscriber can't hang teardown (each also has its own safety deadline).
func drain(wg *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// ---- relay /metrics scraping ----

// relaySnapshot is the subset of the relay's /metrics we track.
type relaySnapshot struct {
	goroutines     float64
	rssBytes       float64
	sessionsActive float64
}

func fetchMetrics(ctx context.Context, client *http.Client, url string) (relaySnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return relaySnapshot{}, fmt.Errorf("build metrics request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return relaySnapshot{}, fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return relaySnapshot{}, fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return relaySnapshot{}, fmt.Errorf("read %s: %w", url, err)
	}
	return parseMetrics(string(body))
}

// parseMetrics extracts the three metrics of interest from Prometheus text
// exposition. Each is an unlabelled scalar, so the line is "name value".
func parseMetrics(text string) (relaySnapshot, error) {
	want := map[string]*float64{}
	var snap relaySnapshot
	want["go_goroutines"] = &snap.goroutines
	want["process_resident_memory_bytes"] = &snap.rssBytes
	want["qumo_relay_sessions_active"] = &snap.sessionsActive
	found := 0
	for line := range strings.SplitSeq(text, "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		name, val, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		dst, tracked := want[name]
		if !tracked {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil {
			return relaySnapshot{}, fmt.Errorf("parse metric %q value %q: %w", name, val, err)
		}
		*dst = f
		found++
	}
	if found == 0 {
		return relaySnapshot{}, errors.New("no expected metrics found in exposition")
	}
	return snap, nil
}

// ---- reporting ----

// verdictFor is the hold criterion: at least 99% of offered sessions must be
// receiving frames. Shared by the console report and the JSONL record so they
// never disagree.
func verdictFor(receiving, sessions int) string {
	if receiving < int(float64(sessions)*0.99) {
		return "CANNOT-HOLD"
	}
	return "HOLDS"
}

func report(target dialTarget, sessions int, r carryResult) {
	verdict := verdictFor(r.receiving, r.launched)
	fmt.Printf("loadgen subscribe → relay %s (path %s)\n", target.relay, target.path)
	fmt.Printf("  offered sessions : %d", sessions)
	if r.rampPerSec > 0 {
		fmt.Printf(" @ %.0f/s ramp", r.rampPerSec)
	}
	fmt.Println()
	if r.launched != sessions {
		fmt.Printf("  launched sessions: %d (admission aborted mid-ramp)\n", r.launched)
	}
	fmt.Printf("  connected        : %d\n", r.connected)
	fmt.Printf("  receiving        : %d\n", r.receiving)
	if r.metricsScraped {
		fmt.Printf("  relay Δgoroutines: %d (%.1f/session)\n", r.relayGoros, r.perSessionGoros)
		fmt.Printf("  relay ΔRSS       : %.1f MB (%.1f KB/session)\n", r.relayRSSMB, r.perSessionKB)
		fmt.Printf("  relay sessions   : %.0f active (relay-reported)\n", r.relaySessions)
	} else {
		fmt.Printf("  relay cost       : unavailable (/metrics unreadable — see warning)\n")
	}
	if r.latSamples > 0 {
		fmt.Printf("  e2e latency      : p50 %s  p95 %s  p99 %s  (%d samples)\n",
			r.latP50.Round(time.Microsecond), r.latP95.Round(time.Microsecond),
			r.latP99.Round(time.Microsecond), r.latSamples)
	}
	fmt.Printf("  verdict          : %s\n", verdict)
}

// jsonlRecord is the capacity-group record the relay-bench dashboard reads, so a
// sweep lands in the same index.html. Resource fields are the RELAY's, not the
// load generator's.
type jsonlRecord struct {
	Bench        string  `json:"bench"`
	Group        string  `json:"group"`
	Config       string  `json:"config"`
	Sessions     int     `json:"sessions"`
	Launched     int     `json:"launched,omitempty"`     // sessions actually launched (< sessions only on mid-ramp abort)
	RampPerSec   float64 `json:"ramp_per_sec,omitempty"` // admission pacing (0 = burst)
	Connected    int     `json:"connected"`
	Receiving    int     `json:"receiving"`
	HeapMB       float64 `json:"heap_mb,omitempty"`
	Goros        int     `json:"goros,omitempty"`
	PerSessionKB float64 `json:"per_session_kb,omitempty"`
	LatP50Ms     float64 `json:"lat_p50_ms,omitempty"`
	LatP95Ms     float64 `json:"lat_p95_ms,omitempty"`
	LatP99Ms     float64 `json:"lat_p99_ms,omitempty"`
	FramesRecv   int64   `json:"frames_recv,omitempty"`
	Verdict      string  `json:"verdict"`
}

func emitJSONL(dir string, sessions int, r carryResult) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("mkdir %q: %w", dir, err)
	}
	verdict := verdictFor(r.receiving, r.launched)
	rec := jsonlRecord{
		Bench: "loadgen", Group: "capacity",
		Config:     "out-of-process/S=" + strconv.Itoa(sessions),
		Sessions:   sessions,
		Launched:   r.launched,
		RampPerSec: r.rampPerSec,
		Connected:  r.connected, Receiving: r.receiving,
		HeapMB: r.relayRSSMB, Goros: r.relayGoros,
		PerSessionKB: r.perSessionKB, Verdict: verdict,
		LatP50Ms:   r.latP50.Seconds() * 1000,
		LatP95Ms:   r.latP95.Seconds() * 1000,
		LatP99Ms:   r.latP99.Seconds() * 1000,
		FramesRecv: r.latSamples,
	}
	f, err := os.OpenFile(filepath.Join(dir, "results.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open results.jsonl: %w", err)
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(rec); err != nil {
		return fmt.Errorf("encode record: %w", err)
	}
	return nil
}
