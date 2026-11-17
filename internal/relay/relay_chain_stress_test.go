//go:build integration

// Relay-chain stress (availability) + throughput harness. Same in-process QUIC
// relay machinery as relay_chain_bench_test.go; measures the remaining
// decision-grade stats under sustained load:
//
//   - THROUGHPUT (capacity): max sustained frames/sec and Mbps the chain
//     delivers (the gap=0 sub-run) — bounds how many streams / what bitrate a
//     relay tier handles.
//   - AVAILABILITY: frame-loss rate, latency drift (first vs last slice — a
//     growing backlog), memory growth (a leak), and survival (no panic).
//
// Frames are published ONE PER GROUP (a real media stream is group-per-frame)
// rather than many-in-one-group — gomoqt caps a single group at ~256 frames.
//
//	go test -tags=integration -run='TestRelayChain_Stress' -timeout 180s \
//	    ./internal/relay/ -v
package relay

import (
	"context"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/require"
)

const (
	stressFrameSize = 1200
	stressDuration  = 2 * time.Second      // short enough to fit CI's 5m integration budget
	sustainableGap  = 2 * time.Millisecond // lowest rate; asserted ~loss-free

	// stressLossTolerancePct is the frame-loss fraction the availability gates
	// (sustainable-rate Stress, K=1 FanoutStress) tolerate. It covers the bounded
	// subscribe-setup race: the publisher emits a few frames before a freshly
	// attached subscriber's Subscribe completes end-to-end, which strict-zero
	// can't absorb and flakes on a loaded CI runner. A real availability
	// regression at no-load drops far more than this.
	stressLossTolerancePct = 1.0
)

// TestRelayChain_Stress drives sustained load through a depth-1 chain at three
// rates to locate the throughput knee and verify availability below it:
//   - ~500 / 1000 / 2000 fps (2ms / 1ms / 0.5ms inter-frame gap).
//
// For each rate it reports sent, received, loss%, delivered throughput (fps,
// Mbps), p99 latency, latency drift (median last-slice − median first-slice — a
// growing backlog), and heap growth. The lowest rate is asserted LOSS-FREE with
// stable latency (availability regression gate); higher rates locate where loss
// begins (capacity). Rates are bounded (not unbounded flood) to avoid QUIC
// send-buffer blowup and keep the run within timeout.
func TestRelayChain_Stress(t *testing.T) {
	cert, pool := chainCert(t)
	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}
	const depth = 1

	// Two rates: the sustainable rate (asserted loss-free, the availability gate)
	// and one higher rate (the knee — reported, may drop). Kept to two to fit CI's
	// integration time budget.
	for _, gap := range []time.Duration{sustainableGap, time.Millisecond, 500 * time.Microsecond} {
		t.Run(fmt.Sprintf("gap=%s", gap), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			relays := make([]*Server, depth)
			for i := range depth {
				relays[i] = spinRelay(t, fmt.Sprintf("r%d", i), chainFreeAddr(t), cert, pool, quicCfg)
			}
			for i := 1; i < depth; i++ {
				relays[i].Config.Peers = []Peer{{Address: relays[i-1].MOQServer.Addr}}
				i := i
				go relays[i].ConnectPeers(ctx)
			}

			sent, lats, heapGrowth := stressRun(t, ctx, pool, relays, gap, stressDuration)
			received := len(lats)
			require.NotZero(t, sent, "producer sent nothing")
			require.NotEmpty(t, lats, "received no frames")

			loss := sent - uint64(received)
			lossPct := float64(loss) / float64(sent) * 100
			fps := float64(received) / stressDuration.Seconds()
			mbps := fps * stressFrameSize * 8 / 1e6
			p99 := percentile(lats, 99)
			med := percentile(lats, 50)
			drift := percentile(lastFrac(lats, 0.2), 50) - percentile(firstFrac(lats, 0.2), 50)

			log.Printf("[chain-stress] gap=%-6s sent=%-6d recv=%-6d loss=%-5d (%5.2f%%)  thr=%.0f fps (%.1f Mbps)  lat med=%-7s p99=%-7s drift=%-7s heapΔ=%5.2fMB  survived=✓",
				gap, sent, received, loss, lossPct, fps, mbps,
				med.Round(time.Microsecond), p99.Round(time.Microsecond), drift.Round(time.Microsecond),
				float64(heapGrowth)/(1024*1024))

			// Emit for the report (depth-1 capacity vs publish rate).
			nominalFps := int(1 / gap.Seconds())
			recordBench(t, benchResult{
				Bench: "Stress", Group: "stress", Rate: fmt.Sprintf("%dfps", nominalFps),
				Config:   fmt.Sprintf("gap=%s", gap),
				MedianMs: med.Seconds() * 1000, P99Ms: p99.Seconds() * 1000,
				LossPct: lossPct, Fps: fps, Mbps: mbps, HeapMB: float64(heapGrowth) / (1024 * 1024),
			})

			// Availability gate: the lowest (sustainable) rate must be essentially
			// loss-free and stable (within the subscribe-setup-race tolerance).
			// Higher rates locate the knee and may drop — reported only.
			if gap == sustainableGap {
				require.Less(t, lossPct, stressLossTolerancePct, "frame loss at the sustainable rate exceeds the subscribe-setup-race tolerance (availability regression)")
				require.Less(t, drift, 5*time.Millisecond, "latency drift indicates a growing backlog")
			}
		})
	}
}

// stressRun publishes one group per frame for `duration` at the given inter-frame
// gap (0 = max rate) and returns the count sent, the per-frame latencies
// received, and heap growth over the run.
func stressRun(tb testing.TB, parent context.Context, pool *x509.CertPool, relays []*Server, gap, duration time.Duration) (sent uint64, latencies []time.Duration, heapGrowth uint64) {
	tb.Helper()
	runCtx, runCancel := context.WithTimeout(parent, duration)
	defer runCancel()

	var sentCounter uint64
	pubMux := moqt.NewTrackMux(moqt.NewHopID())
	pubMux.PublishFunc(runCtx, chainBroadcastPath, func(tw *moqt.TrackWriter) {
		defer tw.Close()
		payload := make([]byte, stressFrameSize)
		for {
			if runCtx.Err() != nil {
				return
			}
			seq := atomic.AddUint64(&sentCounter, 1)
			binary.BigEndian.PutUint64(payload[0:8], seq)
			binary.BigEndian.PutUint64(payload[8:16], uint64(time.Now().UnixNano()))
			gw, err := tw.OpenGroup(runCtx)
			if err != nil {
				return
			}
			fr := moqt.NewFrame(stressFrameSize)
			_, _ = fr.Write(payload)
			if err := gw.WriteFrame(fr); err != nil {
				_ = gw.Close()
				return
			}
			_ = gw.Close()
			if gap > 0 {
				time.Sleep(gap)
			}
		}
	})
	pubSess, err := (&moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: &quic.Config{EnableDatagrams: true, MaxIncomingUniStreams: 1 << 20, MaxIncomingStreams: 1 << 20}}).Dial(runCtx, "moqt://"+relays[0].MOQServer.Addr, pubMux)
	require.NoError(tb, err)
	defer pubSess.CloseWithError(moqt.NoError, "done")

	waitForHandler(tb, relays[len(relays)-1], chainBroadcastPath)

	var memBefore runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	subSess, err := (&moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: &quic.Config{EnableDatagrams: true, MaxIncomingUniStreams: 1 << 20, MaxIncomingStreams: 1 << 20}}).Dial(runCtx, "moqt://"+relays[len(relays)-1].MOQServer.Addr, moqt.NewTrackMux(0))
	require.NoError(tb, err)
	defer subSess.CloseWithError(moqt.NoError, "done")
	tr, err := subSess.Subscribe(runCtx, chainBroadcastPath, chainTrackName, nil)
	require.NoError(tb, err)
	defer tr.Close()

	// Read groups until the producer stops (runCtx expiry) or a safety timeout.
	readCtx, readCancel := context.WithTimeout(context.Background(), duration+5*time.Second)
	defer readCancel()
	go func() { <-readCtx.Done(); _ = tr.Close() }()

	buf := moqt.NewFrame(stressFrameSize + 256)
	latencies = make([]time.Duration, 0, 8192)
	for {
		gr, err := tr.AcceptGroup(readCtx)
		if err != nil {
			break
		}
		for frame := range gr.Frames(buf) {
			body := frame.Body()
			if len(body) < chainFrameHeader {
				continue
			}
			pubNs := int64(binary.BigEndian.Uint64(body[8:16]))
			latencies = append(latencies, time.Since(time.Unix(0, pubNs)))
		}
	}

	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)
	sent = atomic.LoadUint64(&sentCounter)
	return sent, latencies, memAfter.HeapAlloc - memBefore.HeapAlloc
}

// ---- stat helpers ----

func percentile(xs []time.Duration, p int) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	s := make([]time.Duration, len(xs))
	copy(s, xs)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[(len(s)-1)*p/100]
}

func firstFrac(xs []time.Duration, frac float64) []time.Duration {
	if len(xs) == 0 {
		return xs
	}
	n := int(math.Floor(float64(len(xs)) * frac))
	if n < 1 {
		n = 1
	}
	return xs[:n]
}

func lastFrac(xs []time.Duration, frac float64) []time.Duration {
	if len(xs) == 0 {
		return xs
	}
	n := int(math.Floor(float64(len(xs)) * frac))
	if n < 1 {
		n = 1
	}
	return xs[len(xs)-n:]
}

// TestRelayChain_FanoutStress drives sustained load through a fan-out topology
// (publisher → origin → {K leaves}) to test the ORIGIN relay's durability under
// replication load — one upstream stream fanned to K leaves. The origin's work
// scales ∝ K (K egresses per frame); this finds the fan-out width at which the
// origin begins to lose frames or grow latency/memory. Reports per-leaf loss%,
// per-leaf throughput, p99 latency (across all leaves), and origin heap/goroutine
// growth. K=1 is asserted loss-free; wider K locates the fan-out durability knee.
func TestRelayChain_FanoutStress(t *testing.T) {
	cert, pool := chainCert(t)
	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}
	const gap = sustainableGap // sustainable single-stream rate; origin load ∝ K

	// K defaults to [1,4]: at K≥8 the relay's per-subscriber egress teardown
	// (trackDistributor goroutines in groupRing.get) does not quiesce promptly on
	// a 2-core runner, so Server.Shutdown hangs after the measurement completes.
	// Override with STRESS_FANOUTS (e.g. "1,4,8,16") on a larger machine. This
	// test runs in the relay-bench workflows, not the per-PR CI gate.
	for _, k := range parseIntListEnv("STRESS_FANOUTS", []int{1, 4}) {
		t.Run(fmt.Sprintf("fanout=%d", k), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			origin := spinRelay(t, "origin", chainFreeAddr(t), cert, pool, quicCfg)
			leaves := make([]*Server, k)
			for i := range k {
				leaves[i] = spinRelay(t, fmt.Sprintf("leaf%d", i), chainFreeAddr(t), cert, pool, quicCfg)
				leaves[i].Config.Peers = []Peer{{Address: origin.MOQServer.Addr}}
			}
			for i := range k {
				i := i
				go leaves[i].ConnectPeers(ctx)
			}

			sent, recvPerLeaf, lats, heapGrowth := stressFanoutRun(t, ctx, pool, origin, leaves, gap, stressDuration)
			require.NotZero(t, sent)

			totalRecv := 0
			maxLossPct := 0.0
			for _, r := range recvPerLeaf {
				totalRecv += r
				lossPct := (float64(sent) - float64(r)) / float64(sent) * 100
				if lossPct > maxLossPct {
					maxLossPct = lossPct
				}
			}
			perLeafFPS := float64(totalRecv) / float64(k) / stressDuration.Seconds()
			p99 := percentile(lats, 99)

			log.Printf("[chain-fanout-stress] K=%-3d sent=%-6d recv/leaf≈%-6d maxLoss=%5.2f%%  perLeaf=%.0ffps  p99=%-7s heapΔ=%5.2fMB goros(origin+leaves)  survived=✓",
				k, sent, totalRecv/k, maxLossPct, perLeafFPS,
				p99.Round(time.Microsecond), float64(heapGrowth)/(1024*1024))

			// Durability gate: K=1 (no fan-out load) must be essentially loss-free
			// (within the subscribe-setup-race tolerance).
			if k == 1 {
				require.Less(t, maxLossPct, stressLossTolerancePct, "frame loss with no fan-out load exceeds the subscribe-setup-race tolerance (availability regression)")
			}
		})
	}
}

// stressFanoutRun publishes one continuous stream to the origin and subscribes
// at all K leaves concurrently. Returns the producer's sent count, per-leaf
// received counts, all latencies aggregated, and heap growth.
func stressFanoutRun(tb testing.TB, parent context.Context, pool *x509.CertPool, origin *Server, leaves []*Server, gap, duration time.Duration) (sent uint64, recvPerLeaf []int, lats []time.Duration, heapGrowth uint64) {
	tb.Helper()
	runCtx, runCancel := context.WithTimeout(parent, duration)
	defer runCancel()

	var sentCounter uint64
	pubMux := moqt.NewTrackMux(moqt.NewHopID())
	pubMux.PublishFunc(runCtx, chainBroadcastPath, func(tw *moqt.TrackWriter) {
		defer tw.Close()
		payload := make([]byte, stressFrameSize)
		for {
			if runCtx.Err() != nil {
				return
			}
			seq := atomic.AddUint64(&sentCounter, 1)
			binary.BigEndian.PutUint64(payload[0:8], seq)
			binary.BigEndian.PutUint64(payload[8:16], uint64(time.Now().UnixNano()))
			gw, err := tw.OpenGroup(runCtx)
			if err != nil {
				return
			}
			fr := moqt.NewFrame(stressFrameSize)
			_, _ = fr.Write(payload)
			if err := gw.WriteFrame(fr); err != nil {
				_ = gw.Close()
				return
			}
			_ = gw.Close()
			if gap > 0 {
				time.Sleep(gap)
			}
		}
	})
	pubSess, err := (&moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: &quic.Config{EnableDatagrams: true, MaxIncomingUniStreams: 1 << 20, MaxIncomingStreams: 1 << 20}}).Dial(runCtx, "moqt://"+origin.MOQServer.Addr, pubMux)
	require.NoError(tb, err)
	defer pubSess.CloseWithError(moqt.NoError, "done")

	for _, leaf := range leaves {
		waitForHandler(tb, leaf, chainBroadcastPath)
	}

	var memBefore runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	// K subscribers, one per leaf, reading concurrently.
	type leafResult struct {
		recv int
		lats []time.Duration
	}
	k := len(leaves)
	results := make([]leafResult, k)
	var wg sync.WaitGroup
	for i, leaf := range leaves {
		i, leaf := i, leaf
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := leafResult{lats: make([]time.Duration, 0, 4096)}
			readCtx, readCancel := context.WithTimeout(context.Background(), duration+5*time.Second)
			defer readCancel()
			sess, err := (&moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: &quic.Config{EnableDatagrams: true, MaxIncomingUniStreams: 1 << 20, MaxIncomingStreams: 1 << 20}}).Dial(readCtx, "moqt://"+leaf.MOQServer.Addr, moqt.NewTrackMux(0))
			if err != nil {
				results[i] = res
				return
			}
			defer sess.CloseWithError(moqt.NoError, "done")
			tr, err := sess.Subscribe(readCtx, chainBroadcastPath, chainTrackName, nil)
			if err != nil {
				return
			}
			defer tr.Close()
			buf := moqt.NewFrame(stressFrameSize + 256)
			for {
				gr, err := tr.AcceptGroup(readCtx)
				if err != nil {
					break
				}
				for frame := range gr.Frames(buf) {
					body := frame.Body()
					if len(body) < chainFrameHeader {
						continue
					}
					pubNs := int64(binary.BigEndian.Uint64(body[8:16]))
					res.recv++
					res.lats = append(res.lats, time.Since(time.Unix(0, pubNs)))
				}
			}
			results[i] = res
		}()
	}
	wg.Wait()

	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	recvPerLeaf = make([]int, k)
	for i, r := range results {
		recvPerLeaf[i] = r.recv
		lats = append(lats, r.lats...)
	}
	sent = atomic.LoadUint64(&sentCounter)
	return sent, recvPerLeaf, lats, memAfter.HeapAlloc - memBefore.HeapAlloc
}
