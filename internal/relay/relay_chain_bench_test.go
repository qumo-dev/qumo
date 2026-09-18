//go:build integration

// Integration relay-chain performance harness. Tagged `integration` because it
// stands up real in-process QUIC relays (slow); run with:
//
//	go test -run=^$ -tags=integration -bench='BenchmarkRelayChain' \
//	    -benchtime=1x -cpu=1 ./internal/relay/
//
// These are MEASUREMENT benchmarks (one pass per config via -benchtime=1x), not
// regression benchmarks: they report custom metrics (latency / memory / CPU) via
// b.ReportMetric and a log line, not ns/op. The goal is to characterize how
// per-hop latency and per-relay resource cost scale with chain depth and fan-out
// — the data for deciding overlay topology (how deep / how wide a relay mesh can
// be before latency or resource cost dominates).
//
// TLS: one self-signed cert is generated per benchmark and trusted via a RootCAs
// pool (the cert is its own issuer), so verification stays ON — no
// InsecureSkipVerify — for these loopback, in-process connections.
//
// Caveats: numbers are from a dev machine (Windows loopback here; reproduce on
// Linux for CPU + production-like latency); the producer is paced so each frame
// is an individual sample (no in-ring batching); fan-out aggregates across ALL
// leaves. Read the SHAPE across configs, not absolute values.
package relay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/require"
)

const (
	chainBroadcastPath = moqt.BroadcastPath("/bench/chain")
	chainTrackName     = moqt.TrackName("data")
	chainFrameHeader   = 16 // [8 seq][8 publish_unix_nano]
	// chainProduceGap paces the producer. 0 = burst: all frames are written
	// back-to-back in one group and arrive as a batch, so the FIRST frame's
	// latency (min) is the cleanest end-to-end propagation signal (no in-ring
	// queueing). Pacing >0 was tried but on loopback the relay's ~1ms egress
	// poll floor swamps the sub-ms-per-hop propagation, flattening the depth
	// curve — so burst + min is the per-hop signal on loopback. (On a real
	// network with ms-per-hop, pacing would isolate steady-state per-frame
	// latency.)
	chainProduceGap = 0
)

type chainConfig struct {
	label     string
	depth     int // series: relays in the chain
	fanout    int // fan-out: leaf relays
	frameSize int
	numFrames int
}

type chainStats struct {
	samples    []time.Duration
	heapDelta  uint64
	gorosDelta int
	cpuDelta   time.Duration // 0 if unsupported (non-linux/darwin)
}

// ---- helpers ----

func chainFreeAddr(tb testing.TB) string {
	tb.Helper()
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	require.NoError(tb, err)
	c, err := net.ListenUDP("udp", addr)
	require.NoError(tb, err)
	port := c.LocalAddr().(*net.UDPAddr).Port
	require.NoError(tb, c.Close())
	return fmt.Sprintf("127.0.0.1:%d", port)
}

func chainCert(tb testing.TB) (tls.Certificate, *x509.CertPool) {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(tb, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(tb, err)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(tb, err)
	pool := x509.NewCertPool()
	require.True(tb, pool.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func chainDialerTLS(pool *x509.CertPool) *tls.Config {
	return &tls.Config{RootCAs: pool, NextProtos: []string{moqt.NextProtoMOQ}, MinVersion: tls.VersionTLS13}
}

func spinRelay(tb testing.TB, nodeID, addr string, cert tls.Certificate, pool *x509.CertPool, quicCfg *quic.Config) *Server {
	tb.Helper()
	// Sweepable relay knobs (paramexp wiring): RELAY_RING → GroupCacheSize,
	// RELAY_FRAME → FrameCapacity. ≤0 keeps the defaults.
	cfg := &Config{NodeID: nodeID}
	if v := envIntDef("RELAY_RING", 0); v > 0 {
		cfg.GroupCacheSize = v
	}
	if v := envIntDef("RELAY_FRAME", 0); v > 0 {
		cfg.FrameCapacity = v
	}
	s := &Server{
		MOQServer: &moqt.Server{Addr: addr, TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert}, NextProtos: []string{moqt.NextProtoMOQ}, MinVersion: tls.VersionTLS13,
		}, QUICConfig: quicCfg},
		MOQDialer: &moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: quicCfg},
		Config:    cfg,
		// A real (non-zero) per-relay HopID is REQUIRED for announce-loop
		// prevention (excludeHop==0 disables it): without it a ≥3-hop chain
		// re-floods the announcement and hits "duplicated broadcast path".
		TrackMux: moqt.NewTrackMux(moqt.NewHopID()),
	}
	go func() { _ = s.ListenAndServe() }()
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	chainWaitReachable(tb, addr, pool, quicCfg)
	return s
}

func chainWaitReachable(tb testing.TB, addr string, pool *x509.CertPool, quicCfg *quic.Config) {
	tb.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		probe := &moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: quicCfg}
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		sess, err := probe.Dial(ctx, peerURL(addr), moqt.NewTrackMux(0))
		cancel()
		if err == nil {
			_ = sess.CloseWithError(0, "probe")
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	tb.Fatalf("relay %s never became reachable", addr)
}

func waitForHandler(tb testing.TB, s *Server, path moqt.BroadcastPath) {
	tb.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ann, _ := s.TrackMux.TrackHandler(path); ann != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	tb.Fatalf("announcement for %s never propagated to %s", path, s.Config.NodeID)
}

// ---- pub/sub (split so fan-out can attach many subscribers to one publisher) ----

// chainPublish registers a lazy PublishFunc (the producer runs per subscriber)
// and dials pubRelay. The returned session must be kept alive until all
// subscribers have finished, then closed.
func chainPublish(tb testing.TB, pubURL string, pool *x509.CertPool, cfg chainConfig) *moqt.Session {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	tb.Cleanup(cancel)
	pubMux := moqt.NewTrackMux(moqt.NewHopID())
	pubMux.PublishFunc(ctx, chainBroadcastPath, func(tw *moqt.TrackWriter) {
		defer tw.Close()
		gw, err := tw.OpenGroup(ctx)
		if err != nil {
			return
		}
		payload := make([]byte, cfg.frameSize)
		for i := range cfg.numFrames {
			binary.BigEndian.PutUint64(payload[0:8], uint64(i))
			binary.BigEndian.PutUint64(payload[8:16], uint64(time.Now().UnixNano()))
			if i > 0 {
				time.Sleep(chainProduceGap) // pace ⇒ each frame is an individual sample
			}
			fr := moqt.NewFrame(cfg.frameSize)
			_, _ = fr.Write(payload)
			_ = gw.WriteFrame(fr)
		}
		_ = gw.Close()
	})
	sess, err := (&moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: &quic.Config{EnableDatagrams: true, MaxIncomingUniStreams: 1 << 20, MaxIncomingStreams: 1 << 20}}).Dial(ctx, pubURL, pubMux)
	require.NoError(tb, err)
	return sess
}

// chainSubscribe waits for the announcement at subRelay, dials it, subscribes,
// and returns one end-to-end latency sample per frame (arrival − embedded publish
// time; same process clock, no sync needed).
func chainSubscribe(tb testing.TB, subURL string, subRelay *Server, pool *x509.CertPool, cfg chainConfig) []time.Duration {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	waitForHandler(tb, subRelay, chainBroadcastPath)
	sess, err := (&moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: &quic.Config{EnableDatagrams: true, MaxIncomingUniStreams: 1 << 20, MaxIncomingStreams: 1 << 20}}).Dial(ctx, subURL, moqt.NewTrackMux(0))
	require.NoError(tb, err)
	defer sess.CloseWithError(moqt.NoError, "done")

	tr, err := sess.Subscribe(ctx, chainBroadcastPath, chainTrackName, nil)
	require.NoError(tb, err)
	defer tr.Close()
	gr, err := tr.AcceptGroup(ctx)
	require.NoError(tb, err)

	out := make([]time.Duration, 0, cfg.numFrames)
	buf := moqt.NewFrame(cfg.frameSize + 256)
	for frame := range gr.Frames(buf) {
		body := frame.Body()
		if len(body) < chainFrameHeader {
			continue
		}
		pubNs := int64(binary.BigEndian.Uint64(body[8:16]))
		out = append(out, time.Since(time.Unix(0, pubNs)))
	}
	return out
}

// ---- measurement ----

// resourceSnapshot captures process resource usage around a measurement.
type resourceSnapshot struct {
	heap  uint64
	goros int
	cpu   time.Duration
}

func snapshotBefore() resourceSnapshot {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return resourceSnapshot{heap: m.HeapAlloc, goros: runtime.NumGoroutine(), cpu: processCPUTime()}
}

func (a resourceSnapshot) delta(b resourceSnapshot) (heapMB float64, goros int, cpu time.Duration) {
	return float64(b.heap-a.heap) / (1024 * 1024), b.goros - a.goros, b.cpu - a.cpu
}

func measureSeries(tb testing.TB, pool *x509.CertPool, relays []*Server, cfg chainConfig) chainStats {
	tb.Helper()
	pub := chainPublish(tb, "moqt://"+relays[0].MOQServer.Addr, pool, cfg)
	before := snapshotBefore()
	samples := chainSubscribe(tb, "moqt://"+relays[len(relays)-1].MOQServer.Addr, relays[len(relays)-1], pool, cfg)
	after := snapshotBefore()
	_ = pub.CloseWithError(moqt.NoError, "done")
	heapMB, goros, cpu := before.delta(after)
	return chainStats{samples: samples, heapDelta: uint64(heapMB * 1024 * 1024), gorosDelta: goros, cpuDelta: cpu}
}

// measureFanout publishes once at the origin and subscribes at ALL leaves
// concurrently, aggregating per-frame latencies across the whole fan-out.
func measureFanout(tb testing.TB, pool *x509.CertPool, origin *Server, leaves []*Server, cfg chainConfig) chainStats {
	tb.Helper()
	pub := chainPublish(tb, "moqt://"+origin.MOQServer.Addr, pool, cfg)
	before := snapshotBefore()

	var mu sync.Mutex
	var all []time.Duration
	var wg sync.WaitGroup
	for _, leaf := range leaves {
		wg.Add(1)
		leaf := leaf
		go func() {
			defer wg.Done()
			s := chainSubscribe(tb, "moqt://"+leaf.MOQServer.Addr, leaf, pool, cfg)
			mu.Lock()
			all = append(all, s...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	after := snapshotBefore()
	_ = pub.CloseWithError(moqt.NoError, "done")
	heapMB, goros, cpu := before.delta(after)
	return chainStats{samples: all, heapDelta: uint64(heapMB * 1024 * 1024), gorosDelta: goros, cpuDelta: cpu}
}

// ---- benchmarks ----

// BenchmarkRelayChain_Series measures per-hop latency and per-relay resource
// cost vs chain DEPTH (publisher → r1 → … → rN → subscriber). Per-hop latency =
// the slope of median latency vs depth.
func BenchmarkRelayChain_Series(b *testing.B) {
	cert, pool := chainCert(b)
	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}
	for _, depth := range []int{1, 3, 5, 8} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			cfg := chainConfig{label: fmt.Sprintf("series depth=%d", depth), depth: depth, frameSize: 256, numFrames: 50}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			relays := make([]*Server, depth)
			for i := range depth {
				relays[i] = spinRelay(b, fmt.Sprintf("r%d", i), chainFreeAddr(b), cert, pool, quicCfg)
			}
			// r[i] dials r[i-1] (upstream toward the publisher). Raw host:port —
			// maintainPeer adds the moqt:// scheme itself (peerURL).
			for i := 1; i < depth; i++ {
				relays[i].Config.Peers = []Peer{{Address: relays[i-1].MOQServer.Addr}}
			}
			for i := 1; i < depth; i++ {
				i := i
				go relays[i].ConnectPeers(ctx) //nolint:errcheck
			}
			reportChainStats(b, cfg, measureSeries(b, pool, relays, cfg), depth)
		})
	}
}

// BenchmarkRelayChain_Fanout measures leaf latency and origin resource cost vs
// FAN-OUT (publisher → origin → {leaf1..leafK}); subscribes at ALL leaves and
// aggregates.
func BenchmarkRelayChain_Fanout(b *testing.B) {
	cert, pool := chainCert(b)
	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}
	for _, fanout := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("fanout=%d", fanout), func(b *testing.B) {
			cfg := chainConfig{label: fmt.Sprintf("fanout fanout=%d", fanout), fanout: fanout, frameSize: 256, numFrames: 50}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			origin := spinRelay(b, "origin", chainFreeAddr(b), cert, pool, quicCfg)
			leaves := make([]*Server, fanout)
			for i := range fanout {
				leaves[i] = spinRelay(b, fmt.Sprintf("leaf%d", i), chainFreeAddr(b), cert, pool, quicCfg)
				leaves[i].Config.Peers = []Peer{{Address: origin.MOQServer.Addr}} // raw host:port
			}
			for i := range fanout {
				i := i
				go leaves[i].ConnectPeers(ctx) //nolint:errcheck
			}
			reportChainStats(b, cfg, measureFanout(b, pool, origin, leaves, cfg), fanout)
		})
	}
}

// ---- reporting ----

// benchResult is one machine-readable measurement record. When BENCH_RESULTS_DIR
// is set (CI), recordBench appends each record as a JSON line to
// $BENCH_RESULTS_DIR/results.jsonl so the relay-bench report script can build
// CSV artifacts and plots without parsing free-form `go test` output (Go's -json
// stream embeds ReportMetric values only inside output strings). No-op locally.
type benchResult struct {
	Bench    string  `json:"bench"`            // function name, e.g. "FanoutSweep"
	Group    string  `json:"group"`            // series|fanout|load|objsize|soak|reconnect
	Config   string  `json:"config"`           // "K=4", "depth=3", "100fps/K=8", "slice=3"
	K        int     `json:"k,omitempty"`      // fan-out width
	Depth    int     `json:"depth,omitempty"`  // chain depth (series)
	Rate     string  `json:"rate,omitempty"`   // publish rate label (load)
	SizeB    int     `json:"size_b,omitempty"` // frame size bytes (objsize)
	Slice    int     `json:"slice,omitempty"`  // soak time-slice index
	MinMs    float64 `json:"min_ms,omitempty"`
	P25Ms    float64 `json:"p25_ms,omitempty"` // Q1 (boxplot lower)
	MedianMs float64 `json:"median_ms,omitempty"`
	P75Ms    float64 `json:"p75_ms,omitempty"` // Q3 (boxplot upper)
	P95Ms    float64 `json:"p95_ms,omitempty"`
	P99Ms    float64 `json:"p99_ms,omitempty"`
	MaxMs    float64 `json:"max_ms,omitempty"`
	LossPct  float64 `json:"loss_pct,omitempty"`
	Fps      float64 `json:"fps,omitempty"`
	Mbps     float64 `json:"mbps,omitempty"`
	HeapMB   float64 `json:"heap_mb,omitempty"`
	Goros    int     `json:"goros,omitempty"`
	CpuMs    float64 `json:"cpu_ms,omitempty"`
	Fairness float64 `json:"fairness,omitempty"` // Jain's index, 0-1 (1=perfectly fair fan-out)
	JitterMs float64 `json:"jitter_ms,omitempty"`
	// The capacity group (concurrent-session ceiling) is produced out-of-process
	// by `qumo loadgen` (internal/loadgen), which writes its own record shape;
	// the in-process capacity benchmarks were removed as unable to reach the
	// relay's true ceiling (client establishment cost dominated).
}

func recordBench(tb testing.TB, r benchResult) {
	tb.Helper()
	dir := os.Getenv("BENCH_RESULTS_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "results.jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(r) //nolint:errchkjson // best-effort emission
}

// parseIntListEnv parses a comma-separated int list env var (e.g. FANOUT_KS),
// returning the fallback when unset or malformed. Used to let CI trim sweep
// ranges without code changes.
func parseIntListEnv(name string, fallback []int) []int {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	out := make([]int, 0, 8)
	for _, part := range strings.Split(v, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return fallback // malformed → fall back to the documented default
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

func reportChainStats(b *testing.B, cfg chainConfig, st chainStats, hopsOrFanout int) {
	if len(st.samples) == 0 {
		b.Fatalf("no latency samples for %s", cfg.label)
	}
	sort.Slice(st.samples, func(i, j int) bool { return st.samples[i] < st.samples[j] })
	min := st.samples[0]
	p25 := st.samples[(len(st.samples)-1)*25/100]
	median := st.samples[(len(st.samples)-1)*50/100]
	p75 := st.samples[(len(st.samples)-1)*75/100]
	p95 := st.samples[(len(st.samples)-1)*95/100]
	p99 := st.samples[(len(st.samples)-1)*99/100]
	maxLat := st.samples[len(st.samples)-1]

	b.ReportMetric(median.Seconds()*1000, "med_ms")
	b.ReportMetric(min.Seconds()*1000, "min_ms")
	b.ReportMetric(p99.Seconds()*1000, "p99_ms")
	b.ReportMetric(float64(st.heapDelta)/(1024*1024), "heapMB")
	b.ReportMetric(float64(st.gorosDelta), "gorosDelta")
	if st.cpuDelta > 0 {
		b.ReportMetric(st.cpuDelta.Seconds()*1000, "cpu_ms")
	}

	log.Printf("[chain-bench] %-20s n=%-5d min=%-7s med=%-7s p95=%-7s p99=%-7s heapΔ=%-5.2fMB gorosΔ=%-4d cpuΔ=%-7s",
		cfg.label, len(st.samples),
		min.Round(time.Microsecond), median.Round(time.Microsecond), p95.Round(time.Microsecond), p99.Round(time.Microsecond),
		float64(st.heapDelta)/(1024*1024), st.gorosDelta, st.cpuDelta.Round(time.Microsecond))

	// Structured emission for CI CSV/plot generation. group/K vs depth is
	// derived from the config (a fan-out run sets cfg.fanout; a series run sets
	// cfg.depth).
	r := benchResult{
		Bench: "RelayChain",
		MinMs: min.Seconds() * 1000, P25Ms: p25.Seconds() * 1000,
		MedianMs: median.Seconds() * 1000, P75Ms: p75.Seconds() * 1000,
		P95Ms: p95.Seconds() * 1000, P99Ms: p99.Seconds() * 1000,
		MaxMs:  maxLat.Seconds() * 1000,
		HeapMB: float64(st.heapDelta) / (1024 * 1024), Goros: st.gorosDelta,
	}
	if cfg.fanout > 0 {
		r.Group, r.K, r.Config = "fanout", cfg.fanout, fmt.Sprintf("K=%d", cfg.fanout)
	} else {
		r.Group, r.Depth, r.Config = "series", cfg.depth, fmt.Sprintf("depth=%d", cfg.depth)
	}
	if st.cpuDelta > 0 {
		r.CpuMs = st.cpuDelta.Seconds() * 1000
	}
	recordBench(b, r)
}
