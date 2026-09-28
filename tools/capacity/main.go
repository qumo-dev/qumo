// Command capacity measures a qumo relay's concurrent-session capacity by
// driving the `qumo loadgen` primitives. It (optionally) starts a local relay,
// runs a `qumo loadgen publish` trickle source, and probes session counts with
// `qumo loadgen subscribe <N>` — either an explicit --sessions list or, with
// --auto, climbing until the relay can't hold to find the ceiling (--bisect to
// pin the boundary).
//
// Only the relay is a separate process from the load; point --relay at another
// host (with --ca) for a true two-host run, or --start-relay --relay-cores to
// isolate a local relay's CPU as a single-box stand-in. Each probe appends a
// capacity-group record to <results>/results.jsonl, which the relay-bench
// dashboard (scripts/relay_bench_report.ts) renders.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type config struct {
	qumo, relay, caFile, path, track, results string
	hold                                      time.Duration
	gps                                       float64
	size                                      int
	startRelay                                bool
	insecure                                  bool
	relayCores                                string
	gogc                                      int
	ramp                                      float64
}

func run(args []string) error {
	fs := flag.NewFlagSet("capacity", flag.ContinueOnError)
	qumo := fs.String("qumo", "qumo", "qumo binary (path, or a name on $PATH)")
	relay := fs.String("relay", "127.0.0.1:4433", "relay moqt address (host:port)")
	caFile := fs.String("ca", "", "relay cert/CA to trust (required unless --start-relay or --insecure)")
	bpath := fs.String("path", "/bench/carry", "broadcast path")
	track := fs.String("track", "data", "track name")
	hold := fs.Duration("hold", 15*time.Second, "hold duration per probe")
	gps := fs.Float64("gps", 0.5, "publisher groups per second")
	size := fs.Int("size", 64, "frame size in bytes")
	results := fs.String("results", "capacity-results", "dir for results.jsonl (dashboard input)")
	startRelay := fs.Bool("start-relay", false, "spawn a local relay (self-signed cert generated in-process)")
	ramp := fs.Float64("ramp", 0, "session admission pacing in sessions/second passed to loadgen subscribe (0 = burst)")
	insecure := fs.Bool("insecure", false, "skip relay TLS verification, passed to loadgen (dev; self-signed relay)")
	relayCores := fs.String("relay-cores", "", "taskset CPU list for the relay (Linux; --start-relay)")
	gogc := fs.Int("gogc", 800, "GOGC for the relay (--start-relay)")
	sessionsArg := fs.String("sessions", "", `explicit session counts to probe, e.g. "2000 5000 8000"`)
	auto := fs.Bool("auto", false, "climb to find the ceiling instead of a fixed --sessions list")
	startN := fs.Int("start", 2000, "auto: first session count to probe")
	maxN := fs.Int("max", 50000, "auto: upper bound / safety cap")
	step := fs.Int("step", 0, "auto: fixed climb increment (0 = geometric via --growth)")
	growth := fs.Float64("growth", 2.0, "auto: geometric growth factor when --step is 0")
	bisect := fs.Bool("bisect", false, "auto: binary-search the boundary after the first CANNOT-HOLD")
	bisectTol := fs.Int("bisect-tol", 1000, "auto: stop bisection when the HOLD/FAIL gap is <= this")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Exactly one measurement mode.
	if *auto == (*sessionsArg != "") {
		return errors.New(`specify exactly one of --sessions "<list>" or --auto`)
	}
	var sessions []int
	if !*auto {
		var err error
		if sessions, err = parseSessions(*sessionsArg); err != nil {
			return err
		}
	}
	search := ceilingSearch{start: *startN, max: *maxN, step: *step, growth: *growth, bisect: *bisect, tol: *bisectTol}
	if *auto {
		if err := search.validate(); err != nil {
			return err
		}
	}

	cfg := config{
		qumo: *qumo, relay: *relay, caFile: *caFile, path: *bpath, track: *track, results: *results,
		hold: *hold, gps: *gps, size: *size,
		startRelay: *startRelay, insecure: *insecure, relayCores: *relayCores, gogc: *gogc, ramp: *ramp,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return orchestrate(ctx, cfg, sessions, *auto, search)
}

func orchestrate(ctx context.Context, cfg config, sessions []int, auto bool, search ceilingSearch) error {
	var probe func(n int) (bool, error)

	if cfg.startRelay {
		// Local mode: a FRESH relay + publisher per probe. Each probe is an
		// independent measurement — the relay never carries residual state
		// (goroutines/heap) from a prior overload into the next probe, which
		// otherwise contaminates the readings once you push past the ceiling.
		// The cert is generated once and reused as both the relay's server cert
		// and the clients' trust anchor.
		tmp, err := os.MkdirTemp("", "capacity-relay-")
		if err != nil {
			return fmt.Errorf("temp dir: %w", err)
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		certFile, keyFile, err := generateRelayCert(tmp)
		if err != nil {
			return err
		}
		cfg.caFile = certFile
		probe = func(n int) (bool, error) {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			return probeFreshRelay(ctx, cfg, certFile, keyFile, n)
		}
	} else {
		// Remote mode: the relay is a persistent external service we don't own,
		// so we can't cycle it — one publisher for the whole run.
		if cfg.caFile == "" && !cfg.insecure {
			return errors.New("--ca is required unless --start-relay or --insecure")
		}
		if err := waitForMetrics(ctx, metricsURL(cfg), 30*time.Second); err != nil {
			return err
		}
		stopPub, err := startPublisher(ctx, cfg)
		if err != nil {
			return err
		}
		defer stopPub()
		sleepCtx(ctx, 2*time.Second) // let the announcement propagate
		probe = func(n int) (bool, error) {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			return measure(ctx, cfg, n)
		}
	}

	if auto {
		res, err := findCeiling(search, probe)
		if err != nil {
			return err
		}
		printCeiling(search, res)
		return nil
	}
	for _, n := range sessions {
		if ctx.Err() != nil {
			break
		}
		if _, err := probe(n); err != nil {
			return err
		}
	}
	return nil
}

// probeFreshRelay starts a fresh relay + publisher, runs one measurement at N,
// then tears both down (via defers) — so the probe can't inherit state from a
// prior one.
func probeFreshRelay(ctx context.Context, cfg config, certFile, keyFile string, n int) (bool, error) {
	stopRelay, err := startRelay(ctx, cfg, certFile, keyFile)
	if err != nil {
		return false, err
	}
	defer stopRelay()
	if err := waitForMetrics(ctx, metricsURL(cfg), 30*time.Second); err != nil {
		return false, err
	}
	stopPub, err := startPublisher(ctx, cfg)
	if err != nil {
		return false, err
	}
	defer stopPub()
	sleepCtx(ctx, 2*time.Second) // let the announcement propagate
	return measure(ctx, cfg, n)
}

// measure runs `qumo loadgen subscribe <N>` against the current relay and reads
// back the HOLD/CANNOT-HOLD verdict from results.jsonl.
func measure(ctx context.Context, cfg config, n int) (bool, error) {
	if err := runSubscribe(ctx, cfg, n); err != nil {
		return false, err
	}
	held, _, err := lastVerdict(cfg.results)
	if err != nil {
		return false, fmt.Errorf("read verdict for N=%d: %w", n, err)
	}
	return held, nil
}

func metricsURL(cfg config) string { return "http://" + cfg.relay + "/metrics" }

// ---- subprocess drivers ----

func startRelay(ctx context.Context, cfg config, certFile, keyFile string) (func(), error) {
	name, cargs := cfg.qumo, []string{"relay"}
	switch {
	case cfg.relayCores == "":
	case runtime.GOOS != "linux":
		slog.Warn("--relay-cores ignored (taskset is Linux-only)", "goos", runtime.GOOS)
	default:
		if ts, err := exec.LookPath("taskset"); err != nil {
			slog.Warn("--relay-cores ignored (taskset not found)", "cores", cfg.relayCores)
		} else {
			name, cargs = ts, append([]string{"-c", cfg.relayCores, cfg.qumo}, cargs...)
		}
	}
	cmd := exec.CommandContext(ctx, name, cargs...)
	cmd.Env = append(os.Environ(),
		"RELAY_ADDR="+cfg.relay, "CERT_FILE="+certFile, "KEY_FILE="+keyFile,
		"RELAY_NAME=capacity", "GOGC="+strconv.Itoa(cfg.gogc),
	)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start relay: %w", err)
	}
	slog.Info("started relay", "addr", cfg.relay, "pid", cmd.Process.Pid, "cores", cfg.relayCores, "gogc", cfg.gogc)
	return func() { stopProc(cmd) }, nil
}

// loadgenTLSArgs returns the TLS flags to pass to a spawned loadgen process:
// --insecure when configured, otherwise --ca with the trust anchor.
func loadgenTLSArgs(cfg config) []string {
	if cfg.insecure {
		return []string{"--insecure"}
	}
	return []string{"--ca", cfg.caFile}
}

func startPublisher(ctx context.Context, cfg config) (func(), error) {
	args := []string{"loadgen", "publish", "--relay", cfg.relay}
	args = append(args, loadgenTLSArgs(cfg)...)
	args = append(args, "--path", cfg.path, "--track", cfg.track,
		"--gps", strconv.FormatFloat(cfg.gps, 'f', -1, 64), "--size", strconv.Itoa(cfg.size))
	cmd := exec.CommandContext(ctx, cfg.qumo, args...)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start publisher: %w", err)
	}
	slog.Info("started publisher", "pid", cmd.Process.Pid)
	return func() { stopProc(cmd) }, nil
}

func runSubscribe(ctx context.Context, cfg config, n int) error {
	args := []string{"loadgen", "subscribe", "--relay", cfg.relay}
	args = append(args, loadgenTLSArgs(cfg)...)
	if cfg.ramp > 0 {
		args = append(args, "--ramp", strconv.FormatFloat(cfg.ramp, 'f', -1, 64))
	}
	args = append(args, "--path", cfg.path, "--track", cfg.track,
		"--hold", cfg.hold.String(), "--results", cfg.results,
		strconv.Itoa(n)) // positional N (after flags)
	cmd := exec.CommandContext(ctx, cfg.qumo, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr // surface the per-probe report
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("subscribe %d: %w", n, err)
	}
	return nil
}

// stopProc terminates a child gracefully (SIGTERM, then Kill after a grace
// period) and reaps it.
func stopProc(cmd *exec.Cmd) {
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

// ---- helpers ----

func waitForMetrics(ctx context.Context, url string, timeout time.Duration) error {
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return fmt.Errorf("relay /metrics did not come up at %s within %s", url, timeout)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// parseSessions parses a space/comma-separated list of positive session counts.
func parseSessions(s string) ([]int, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' || r == '\n' })
	if len(fields) == 0 {
		return nil, errors.New("no session counts given")
	}
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid session count %q (want a positive integer)", f)
		}
		out = append(out, n)
	}
	return out, nil
}

// capRecord is the subset of the loadgen capacity JSONL record the driver reads
// back to decide HOLD vs CANNOT-HOLD.
type capRecord struct {
	Sessions  int    `json:"sessions"`
	Connected int    `json:"connected"`
	Receiving int    `json:"receiving"`
	Verdict   string `json:"verdict"`
}

// parseLastRecord returns the last JSON record in a results.jsonl body.
func parseLastRecord(data []byte) (capRecord, error) {
	var last capRecord
	found := false
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var r capRecord
		if err := json.Unmarshal(line, &r); err != nil {
			return capRecord{}, fmt.Errorf("parse record %q: %w", line, err)
		}
		last, found = r, true
	}
	if !found {
		return capRecord{}, errors.New("no records in results.jsonl")
	}
	return last, nil
}

// lastVerdict reads the most recent capacity record and reports whether it held.
func lastVerdict(dir string) (bool, capRecord, error) {
	data, err := os.ReadFile(filepath.Join(dir, "results.jsonl"))
	if err != nil {
		return false, capRecord{}, err
	}
	rec, err := parseLastRecord(data)
	if err != nil {
		return false, capRecord{}, err
	}
	return rec.Verdict == "HOLDS", rec, nil
}
