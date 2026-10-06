package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// subMsg carries the result of one subscriber group back to the orchestrator.
type subMsg struct {
	idx int
	res *SubResult
	err error
}

// RunCell executes one (P, X) experiment: start hub + P edges, start publisher,
// launch subscribers, scrape metrics, and return results.
func RunCell(ctx context.Context, cfg *Config, qumoBin, certDir string) (*CellResult, error) {
	start := time.Now()
	slog.Info("running cell",
		"P", cfg.P, "X", cfg.X,
		"total_subs", cfg.TotalSubs(),
		"gps", cfg.GPS, "frame_size", cfg.FrameSize, "hold", cfg.Hold,
	)

	top := BuildTopology(cfg)

	// ---- Step 1: Kill leftover processes on our ports ----
	for _, r := range top.AllRelays() {
		killPortProcesses(r.Port)
	}
	time.Sleep(500 * time.Millisecond)

	// ---- Step 2: Generate TLS certs ----
	certs, err := EnsureCerts(certDir)
	if err != nil {
		return nil, fmt.Errorf("cert generation: %w", err)
	}
	var names []string
	for _, r := range top.AllRelays() {
		names = append(names, r.Name)
	}
	if err := EnsurePeerCredentials(certDir, names); err != nil {
		return nil, fmt.Errorf("peer credentials: %w", err)
	}

	// ---- Step 3: Start hub ----
	slog.Info("starting hub")
	hubPS, err := startRelay(ctx, qumoBin, top.Hub, certDir, top)
	if err != nil {
		return nil, fmt.Errorf("start hub: %w", err)
	}
	defer stopRelay(hubPS, 5*time.Second)

	if err := waitReady(ctx, hubPS, 30*time.Second); err != nil {
		return nil, fmt.Errorf("hub not ready: %w", err)
	}

	// ---- Step 4: Start edges ----
	slog.Info("starting edges", "count", cfg.P)
	edgePS := make([]*ProcessState, cfg.P)
	for i := range edgePS {
		e := top.Edges[i]
		ps, err := startRelay(ctx, qumoBin, e, certDir, top)
		if err != nil {
			return nil, fmt.Errorf("start edge %d: %w", i, err)
		}
		edgePS[i] = ps
		defer stopRelay(ps, 5*time.Second)
	}

	// Wait for all edges to become ready.
	for i, ps := range edgePS {
		if err := waitReady(ctx, ps, 30*time.Second); err != nil {
			return nil, fmt.Errorf("edge %d not ready: %w", i, err)
		}
	}

	// ---- Step 5: Wait for peer connections (edges → hub) ----
	slog.Info("waiting for peer connections")
	for i, ps := range edgePS {
		if err := waitPeerConnected(ctx, ps, 30*time.Second); err != nil {
			slog.Warn("peer connection check", "edge", i, "err", err)
		}
	}

	// ---- Step 6: Start publisher (subprocess) ----
	slog.Info("starting publisher", "hub", fmt.Sprintf("127.0.0.1:%d", cfg.HubPort))
	pubCancel, err := PublishSubprocess(ctx, qumoBin,
		fmt.Sprintf("127.0.0.1:%d", cfg.HubPort),
		certs.Cert,
		"/bench/carry",
		"data",
		cfg.GPS,
		cfg.FrameSize,
	)
	if err != nil {
		return nil, fmt.Errorf("start publisher: %w", err)
	}
	defer pubCancel()

	// ---- Step 7: Wait for broadcast on hub ----
	slog.Info("waiting for hub broadcast")
	if err := waitBroadcastActive(ctx, hubPS, 30*time.Second); err != nil {
		slog.Warn("hub broadcast check", "err", err)
		// Continue anyway — the publisher may still be initializing.
	}

	// ---- Step 8: Wait for broadcast propagation to edges ----
	slog.Info("waiting for broadcast propagation")
	for i, ps := range edgePS {
		if err := waitBroadcastActive(ctx, ps, 30*time.Second); err != nil {
			slog.Warn("broadcast propagation check", "edge", i, "err", err)
		}
	}
	time.Sleep(3 * time.Second) // let stream state propagate

	// ---- Step 9a: Pre-warm latency probe (before main subscriber batch) ----
	// Launch the latency subscriber early (while the relay is nearly empty) so
	// it connects, subscribes, and starts collecting fresh frames BEFORE the
	// thundering herd of main subscribers arrives. This avoids the stale-frame
	// artifact where a late-starting subscriber receives old buffered groups
	// from the ring cache and reports multi-second latency.
	//
	// The probe runs in its own subprocess with N=1, --latency, and the same
	// hold duration as the main subscribers. It runs concurrently through
	// Step 9–12, and we read its result at Step 13.
	var latProbeRes *SubResult
	latProbeCh := make(chan *SubResult, 1)
	if cfg.LatencyProbe && len(edgePS) > 0 {
		slog.Info("pre-warming latency probe on edge 0 (before main subscriber batch)",
			"port", edgePS[0].Node.Port)
		go func() {
			latAddr := fmt.Sprintf("127.0.0.1:%d", edgePS[0].Node.Port)
			res, err := SubscribeGroupSubprocess(ctx, qumoBin, latAddr, certs.Cert,
				"/bench/carry", "data", 1, cfg.Hold, true,
			)
			if err != nil {
				slog.Warn("pre-warmed latency probe failed", "err", err)
				latProbeCh <- nil
				return
			}
			latProbeCh <- res
		}()
	}

	// ---- Step 9b: Snapshot before metrics ----
	before, err := scrapeAll(ctx, top, hubPS, edgePS)
	if err != nil {
		slog.Warn("pre-scrape metrics", "err", err)
	}

	// ---- Step 10: Launch subscribers (one group per edge, simultaneously) ----
	slog.Info("launching subscribers", "per_edge", cfg.X, "total", cfg.TotalSubs())

	subCh := make(chan *subMsg, cfg.P)

	for i, ps := range edgePS {
		go func(idx int, ps *ProcessState) {
			addr := fmt.Sprintf("127.0.0.1:%d", ps.Node.Port)
			res, err := SubscribeGroupSubprocess(ctx, qumoBin, addr, certs.Cert,
				"/bench/carry", "data", cfg.X, cfg.Hold)
			subCh <- &subMsg{idx: idx, res: res, err: err}
		}(i, ps)
	}

	// Collect subscriber results.
	subResults := make(map[int]*SubResult)
	for range cfg.P {
		r := <-subCh
		if r.err != nil {
			slog.Warn("subscriber group error", "edge", r.idx, "err", r.err)
			continue
		}
		subResults[r.idx] = r.res
	}

	// ---- Step 11: Snapshot after metrics ----
	time.Sleep(2 * time.Second)
	after, err := scrapeAll(ctx, top, hubPS, edgePS)
	if err != nil {
		slog.Warn("post-scrape metrics", "err", err)
	}

	// ---- Step 12: Build result ----
	result := buildResult(cfg, before, after, subResults, start)

	// ---- Step 13: Read pre-warmed latency probe result ----
	// The probe started before the main subscriber batch (Step 9a) and ran
	// concurrently through the hold period. By now it should have finished;
	// we read the result with a short timeout as a safety net.
	if cfg.LatencyProbe {
		select {
		case latProbeRes = <-latProbeCh:
		case <-time.After(3 * time.Second):
			slog.Warn("latency probe did not finish in time — using best-effort result")
		}
	}

	if latProbeRes != nil && latProbeRes.LatencySamples > 0 {
		slog.Info("e2e latency from pre-warmed probe",
			"samples", latProbeRes.LatencySamples,
			"p50_ms", fmt.Sprintf("%.3f", latProbeRes.LatencyP50Ms),
			"p95_ms", fmt.Sprintf("%.3f", latProbeRes.LatencyP95Ms),
			"p99_ms", fmt.Sprintf("%.3f", latProbeRes.LatencyP99Ms),
		)
		result.LatencySamples = latProbeRes.LatencySamples
		result.LatencyP50Ms = latProbeRes.LatencyP50Ms
		result.LatencyP95Ms = latProbeRes.LatencyP95Ms
		result.LatencyP99Ms = latProbeRes.LatencyP99Ms
		result.LatencyMinMs = latProbeRes.LatencyMinMs
		result.LatencyMaxMs = latProbeRes.LatencyMaxMs
		result.LatencyMeanMs = latProbeRes.LatencyMeanMs
	} else if cfg.LatencyProbe {
		slog.Warn("latency probe: no samples collected")
	}

	slog.Info("cell complete",
		"P", cfg.P, "X", cfg.X,
		"connected", result.Connected, "receiving", result.Receiving,
		"sustained", result.Sustained,
		"duration", time.Since(start).Round(time.Second),
	)

	return result, nil
}

// scrapeAll collects metrics from all relay processes at once.
type scrapeSet struct {
	hub   *RelaySnapshot
	edges []*RelaySnapshot
}

func scrapeAll(ctx context.Context, top *Topology, hubPS *ProcessState, edgePS []*ProcessState) (*scrapeSet, error) {
	hub, err := ScrapeRelay(ctx, hubPS.Node.Port)
	if err != nil {
		return nil, fmt.Errorf("scrape hub: %w", err)
	}
	edges := make([]*RelaySnapshot, len(edgePS))
	for i, ps := range edgePS {
		e, err := ScrapeRelay(ctx, ps.Node.Port)
		if err != nil {
			slog.Warn("scrape edge", "i", i, "err", err)
			continue
		}
		edges[i] = e
	}
	return &scrapeSet{hub: hub, edges: edges}, nil
}

// buildResult combines before/after snapshots and subscriber results into a CellResult.
func buildResult(cfg *Config, before, after *scrapeSet, subResults map[int]*SubResult, start time.Time) *CellResult {
	r := &CellResult{
		P:         cfg.P,
		X:         cfg.X,
		TotalSubs: cfg.TotalSubs(),
		Sustained: true,
	}

	// --- Hub metrics ---
	if before != nil && after != nil && before.hub != nil && after.hub != nil {
		r.Hub = &RelayMetrics{
			CPUDeltaS:   clampPos(after.hub.CPUSeconds - before.hub.CPUSeconds),
			RSSMB:       clampPos(after.hub.RSSBytes / 1_000_000),
			HeapMB:      clampPos(after.hub.HeapAllocBytes / 1_000_000),
			Goros:       after.hub.Goroutines,
			Sessions:    after.hub.SessionsActive,
			EgressBytes: int64(clampPos(after.hub.EgressBytesTotal - before.hub.EgressBytesTotal)),
			GCMaxMS:     after.hub.GCDurationMax * 1000,
			GCCount:     int64(clampPos(after.hub.GCDurationCount - before.hub.GCDurationCount)),
			GCCPUS:      clampPos(after.hub.GCDurationSecSum - before.hub.GCDurationSecSum),
		}
		r.HubCPUS = r.Hub.CPUDeltaS
		r.HubSessions = r.Hub.Sessions
	}

	// --- Edge metrics ---
	r.AllEdgesActive = true
	var totalCPU float64
	var totalEgress int64
	var peakRSS float64

	for i := range cfg.P {
		em := &RelayMetrics{}
		if before != nil && after != nil && i < len(before.edges) && i < len(after.edges) {
			b := before.edges[i]
			a := after.edges[i]
			if b != nil && a != nil {
				em = &RelayMetrics{
					CPUDeltaS:         clampPos(a.CPUSeconds - b.CPUSeconds),
					RSSMB:             clampPos(a.RSSBytes / 1_000_000),
					HeapMB:            clampPos(a.HeapAllocBytes / 1_000_000),
					Goros:             a.Goroutines,
					Sessions:          a.SessionsActive,
					SubscribersActive: a.SubscribersActive,
					SubscriberSkips:   int64(clampPos(a.SubscriberSkips - b.SubscriberSkips)),
					EgressBytes:       int64(clampPos(a.EgressBytesTotal - b.EgressBytesTotal)),
					GCMaxMS:           a.GCDurationMax * 1000,
					GCCount:           int64(clampPos(a.GCDurationCount - b.GCDurationCount)),
					GCCPUS:            clampPos(a.GCDurationSecSum - b.GCDurationSecSum),
				}
			}
		}

		// Embed subscriber results.
		if sr, ok := subResults[i]; ok {
			em.Connected = sr.Connected
			em.Receiving = sr.Receiving
		}

		r.Edges = append(r.Edges, em)

		totalCPU += em.CPUDeltaS
		totalEgress += em.EgressBytes
		if em.RSSMB > peakRSS {
			peakRSS = em.RSSMB
		}

		// Check edge participation: the relay's own metrics (egress bytes flowing
		// and active subscriber count) are the authoritative signal. Use a minimum
		// threshold (≥5 active subscribers OR ≥1KB egress) to avoid false negatives
		// from scrape-timing race conditions where a near-perfect edge briefly
		// reports zero during shutdown interleaving. The subscriber-side receiving
		// count is NOT used here because transient subscriber errors (e.g. a single
		// dial timeout) can set it to 0 even while the relay is actively forwarding
		// data to other subscribers on that edge.
		if em.EgressBytes < 1000 && em.SubscribersActive < 5 {
			r.AllEdgesActive = false
		}
	}

	r.AggCPUS = totalCPU
	r.AggEgressBytes = totalEgress
	r.PeakRSSMB = peakRSS

	// --- Aggregate subscribers ---
	r.Connected = 0
	r.Receiving = 0
	for _, sr := range subResults {
		r.Connected += sr.Connected
		r.Receiving += sr.Receiving
	}

	// --- Sustainability check ---
	var reasons []string
	targetConn := cfg.TotalSubs() * 95 / 100
	if r.Connected < targetConn {
		reasons = append(reasons, fmt.Sprintf("connected<%d%%", r.Connected*100/cfg.TotalSubs()))
	}
	if r.Connected > 0 {
		targetRecv := r.Connected * 95 / 100
		if r.Receiving < targetRecv {
			reasons = append(reasons, fmt.Sprintf("receiving<%d%%", r.Receiving*100/r.Connected))
		}
	}
	if !r.AllEdgesActive {
		reasons = append(reasons, "inactive_edges")
	}
	if len(reasons) > 0 {
		r.Sustained = false
		for i, re := range reasons {
			if i > 0 {
				r.StopReasons += ", "
			}
			r.StopReasons += re
		}
	}

	// Measured wall time of the whole cell (setup + hold + teardown), not a
	// constant derived from the hold duration.
	r.WallDuration = fmt.Sprintf("%ds", int(time.Since(start).Seconds()))
	return r
}

// clampPos returns v if v > 0, otherwise 0.
func clampPos(v float64) float64 {
	if v > 0 {
		return v
	}
	return 0
}
