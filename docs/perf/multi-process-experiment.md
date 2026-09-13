# Multi-process scaling experiment

## Question

Does running multiple independent qumo relay processes on a single VM
provide meaningful aggregate-throughput improvement over one process, or
does the bottleneck lie in shared infrastructure that no amount of
process-level isolation can bypass?

## Hypothesis

Multiple relay processes *may* improve throughput by isolating:

- Go runtime scheduler (P/M/G contention across relay and subscriber goros)
- GC scanning (each process scans only its own heap & stacks)
- Heap growth (per-process heap, independent GC pacing)
- OS thread scheduling (process-level time-slice preemption)

But it *will not* help if the bottleneck is shared infrastructure:

- UDP socket processing (single OS-level recv/send queue)
- Kernel networking stack (softirq, NIC ring buffer)
- Memory bandwidth (shared DDR bus)
- CPU saturation (same physical cores, same L2/L3 cache)

The experiment must separate these cases.

---

# Experiment matrix

## Dimension 1: Process count (P)

| Config | Description | Core assignment |
|--------|-------------|-----------------|
| P=1    | 1 relay process (baseline) | All 8 cores |
| P=2    | 2 independent relays | 4 cores each |
| P=4    | 4 independent relays | 2 cores each |
| P=8    | 8 independent relays | 1 core each |

## Dimension 2: Topology class

| Class | Description | Why |
|-------|-------------|-----|
| **Independent** | Each relay has its OWN publisher. Subscribers split evenly. **No inter-relay messages.** | Isolates pure per-process throughput — the fairest test. |
| **Fanned** | ONE publisher → ONE relay (hub) → P-1 edge relays. Subscribers at edges. | Matches the existing hierarchy study (scaling.md); isolates whether hub-plus-edges on one host is different from N independent relays. |

## Dimension 3: Subscriber load

Swept to find the sustainable ceiling per config:

```
N = { 500, 1000, 1500, 2000, 3000, 4000, 6000, 8000 }
```

The ceiling is the highest N where ALL relays remain within SLO
(p99 \< 300 ms, loss \< 1 %).

## Primary comparison

The headline comparison is **P=1 vs P=4** at the **same total subscriber
count**, with **same total cores** reserved. This is the fairest test of the
hypothesis:

```
P=1: 1 relay on 8 cores → serves N subscribers
P=4: 4 relays, 2 cores each → each serves N/4 subscribers, aggregate N
```

If aggregate throughput at N > 1000 is higher for P=4 than P=1, process
isolation helps. If both hit the same ceiling, shared infrastructure is the
limit.

---

# Detailed test scenarios

## Scenario A: Independent relays

```
           ┌──────────┐          ┌──────────┐
Pub1 ─────→│ Relay 1  │───→ Subs1..N/4  (port 4433)
           ├──────────┤          ├──────────┤
Pub2 ─────→│ Relay 2  │───→ SubsN/4+1..N/2 (port 4434)
           ├──────────┤          ├──────────┤
Pub3 ─────→│ Relay 3  │───→ SubsN/2+1..3N/4 (port 4435)
           ├──────────┤          ├──────────┤
Pub4 ─────→│ Relay 4  │───→ Subs3N/4+1..N (port 4436)
           └──────────┘          └──────────┘
```

- Each relay is a completely independent `qumo relay` process.
- Each has its own publisher (`qumo loadgen publish` on a dedicated replica).
  All publishers emit the same workload (30 fps, 1200 B, one frame/group).
- Subscribers are evenly distributed across relays.
- **No peer connections** between relays.

**What this tests:** pure process-level isolation — separate Go heaps,
schedulers, GC cycles, kernel socket buffers.

## Scenario B: Hubbed relays

```
                     ┌──────────┐
Pub ────────────────→│ Hub      │───→ (not serving subs directly)
                     │ (4433)   │
                     └────┬─────┘
                          │ peer links
          ┌───────────────┼───────────────┐
          │               │               │
    ┌─────▼─────┐  ┌─────▼─────┐  ┌─────▼─────┐
    │ Edge 1    │  │ Edge 2    │  │ Edge 3    │  ...
    │ (4434)    │  │ (4435)    │  │ (4436)    │
    └───────────┘  └───────────┘  └───────────┘
```

- One publisher → one hub → K edge relays.
- Subscribers are evenly distributed across edges only.
- This is the existing hierarchy topology (scaling.md), included for
  continuity.

**What this tests:** whether the hub's fan-in/fan-out adds overhead that a
peer-less independent setup avoids.

## Control: Pure CPU budget test

To distinguish "process isolation helps" from "more cores per relay helps":

| Sub-config | Relays | Cores/relay | Total cores |
|------------|--------|-------------|-------------|
| P=1, all 8 | 1      | 8           | 8           |
| P=2, 4+4   | 2      | 4           | 8           |
| P=2, 2+6   | 2      | 2, 6        | 8           |
| P=4, 2+2+2+2 | 4    | 2           | 8           |
| P=4, 4+4   | 4      | 4           | **16**      |

The last row (P=4, 4 cores each, 16 total) answers: *given enough cores, can
multiple relays each hit the single-relay ceiling?* If P=4 on 16 cores serves
4× more than P=1 on 8 cores, the process-isolation hypothesis is supported;
if not, the bottleneck is truly shared (kernel, NIC, memory bandwidth).

---

# Workload definition

| Parameter | Value | Note |
|-----------|-------|------|
| Frame cadence | 30 fps (33 ms gap) | Audio baseline; matches existing data |
| Frames per group | 1 | Worst case for stream-open rate |
| Frame size | 1200 B | Conservative; matches baseline.md |
| Group-open rate | 30 groups/s | = frames/s |
| Publisher | Paced, not bursted | Verified inter-arrival |
| Subscriber hold | 20 s | Steady-state measurement window |
| Settle | 5 s | Discard ramp-up transients |
| SLO | p99 \< 300 ms, loss \< 1 % | Matches baseline.md |

---

# Measurement plan

## Per-relay metrics (scraped from /metrics)

| Metric | Raw source | Derivation |
|--------|------------|------------|
| CPU cores | `process_cpu_seconds_total` | Δ / Δt |
| RSS | `process_resident_memory_bytes` | snapshot |
| Goroutines | `go_goroutines` | snapshot |
| GC p99 | `go_gc_duration_seconds{quantile="1"}` | snapshot |
| GC CPU | `go_gc_duration_seconds_sum` | Δ / Δt (fraction of CPU) |
| GC count | `go_gc_duration_seconds_count` | Δ / Δt |
| Mallocs | `go_memstats_mallocs_total` | Δ / Δt |
| Egress bytes | `qumo_relay_egress_bytes_total` | Δ × 8 / Δt → Mbps |
| Ingress bytes | `qumo_relay_ingress_bytes_total` | Δ × 8 / Δt → Mbps |
| Sessions active | `qumo_relay_sessions_active` | peak during run |
| Subscriber skips | `qumo_relay_subscriber_skips_total` | Δ / Δt |

## Aggregate metrics

| Metric | Calculation |
|--------|-------------|
| Total subscribers served | Sum of `connected` across all loadgen runs |
| Aggregate throughput | Sum of per-relay egress Mbps |
| Total relay CPU | Sum of per-relay CPU cores |
| System CPU | `mpstat` / `top` for the whole host |
| Loss % | Per-group: `(total_sent - total_received) / total_sent` |
| Latency p50/p95/p99 | End-to-end from publisher timestamp (per subscriber) |

## System-level metrics

| Metric | Source | What it reveals |
|--------|--------|-----------------|
| UDP drops | `/proc/net/udp` or `netstat -s` | recv-buffer overflow |
| CPU steal | `/proc/stat` or `mpstat` | hypervisor contention |
| Context switches | `/proc/stat` | scheduler overhead |
| Memory bandwidth | `perf stat -e` or `numastat` | shared-memory bus |

---

# Implementation

## Files (all in `bench-multiproc/`)

| File | Purpose |
|------|---------|
| `run-sweep.sh` | Top-level orchestrator: loops over P and N, calls `run-level.sh` for each cell |
| `run-level.sh` | Runs one (P, N) cell: starts/waits/stops relays, publishers, subscribers; scrapes /metrics; writes JSONL |
| `analyze.sh` | Parses results.jsonl into a formatted summary table with efficiency analysis |
| `gen-cert.sh` | Generates a self-signed ECDSA cert (openssl or Go fallback) for localhost |
| `results/` | Output directory for per-cell log files and results.jsonl |

## `run-level.sh` workflow

```
1. Kill any leftover processes on target ports (fuser -k / netstat)
2. Generate self-signed cert if needed
3. Start P relay processes with taskset pinning (if available)
4. Wait for ALL relays to be reachable (/health endpoint, up to 30s)
5. Start P publisher processes (one per relay, taskset-pinned to loadgen cores)
6. Wait for ALL publishers to register broadcasts (check qumo_relay_broadcasts_active)
7. Snapshot per-relay /metrics (baseline: CPU, RSS, heap, goros, GC, egress counters)
8. Launch subscriber processes CONCURRENTLY across all relays (each relay gets N/P subs)
9. Wait for all subscriber processes to finish (loadgen subscribe --hold <HOLD>)
10. Snapshot per-relay /metrics (steady state)
11. Compute per-relay deltas (CPU, egress bytes) and snapshots (RSS, heap, goros, sessions, GC max)
12. Collect system-level UDP drop counters
13. Determine sustainability (>=95% connected, >=95% receiving)
14. Append JSONL record to results/results.jsonl
15. Kill all processes (trap EXIT handler)
```

## Core assignment

On an N-core host:
- **60 % of cores** reserved for relays (evenly split across P processes)
- **40 % of cores** for load generators (publishers + subscribers)

| P | 8-core host relay masks | Loadgen mask |
|---|------------------------|--------------|
| 1 | `0-4` (5 cores) | `5-7` |
| 2 | `0-2`, `3-4` | `5-7` |
| 4 | `0`, `1`, `2`, `3` | `4-7` |
| 8 | `0`-`7` (1 each, no room for loadgen) | `8-7` (empty — skipped) |

## Relay configuration

```
RELAY_ADDR=127.0.0.1:<port>       # unique port per relay
CERT_FILE=cert.pem, KEY_FILE=key.pem  # self-signed dev cert
CA_FILE=cert.pem                   # same file is both cert and CA
RELAY_GOGC=800                     # match existing bench config
GROUP_CACHE_SIZE=8                 # default
--role (unset)                     # flat mode (no hub/edge)
```

## Metrics collected per cell

### From /metrics (before/after delta)

| Metric | Source | Type |
|--------|--------|------|
| CPU time | `process_cpu_seconds_total` | Δ cumulative |
| RSS | `process_resident_memory_bytes` | snapshot |
| Go heap | `go_memstats_heap_alloc_bytes` | snapshot |
| Goroutines | `go_goroutines` | snapshot |
| Sessions active | `qumo_relay_sessions_active` | snapshot |
| Egress bytes | `qumo_relay_egress_bytes_total` | Δ cumulative |
| GC max pause | `go_gc_duration_seconds{quantile="1"}` | snapshot |
| GC CPU | `go_gc_duration_seconds_sum` | Δ cumulative |
| GC count | `go_gc_duration_seconds_count` | Δ cumulative |

### From loadgen subscribe output

| Metric | Source |
|--------|--------|
| Connected sessions | `connected : N` |
| Receiving sessions | `receiving : N` |

### System-level

| Metric | Source |
|--------|--------|
| UDP receive drops | `/proc/net/udp` (field 13, Linux) or `netstat -s -u` |
| CPU utilization | `mpstat` (when available) |

### Known gaps (not measured)

1. **E2E latency** — The `qumo loadgen subscribe` tool does not report per-frame
   end-to-end latency. For latency measurements, use the instrument build
   (`-tags=instrument`) or the in-process benchmark (`single_relay_bench_test.go`).
2. **Frame-level throughput** — The loadgen tool reports session counts, not
   per-frame delivery rates. Egress byte counters provide aggregate throughput.

## Output schema (results.jsonl)

```jsonl
{"P":1,"N":500,"per_relay":500,"connected":498,"receiving":498,
 "agg_cpu_s":0.88,"agg_egress_bytes":45000000,"peak_rss_mb":245.0,
 "udp_drop_delta":0,"udp_err_delta":0,"wall_s":30,
 "sustained":true,"stop_reasons":"",
 "relays":{"relay0":{"cpu_delta_s":0.88,"rss_mb":245.0,"heap_mb":120.5,
   "goros":3450,"sessions":498,"egress_bytes":45000000,"gc_max_ms":2.1}}}
```

---

# Analysis plan

## If process isolation helps

We expect to see:

```
Aggregate throughput at SLO:
  P=1: ~1000 subscribers (baseline)
  P=2: >1000 subscribers (maybe 1500-1800)
  P=4: >1800 subscribers (maybe 2500-3500)
  P=8: >3500 subscribers
```

Key signatures:

| Evidence | What it means |
|----------|---------------|
| Per-relay CPU cores sum ≈ total CPU | Processes share cores, but individually use less than the single-process ceiling. |
| Per-relay GC CPU % < single-process GC % | GC scanning is isolated — each smaller heap scans faster. |
| Per-relay p99 stays below SLO at higher aggregate N | Each quic-go event loop set runs its own P, reducing scheduler contention. |
| P=4 on 16 cores ≈ 4× P=1 on 4 cores | Linear process scaling: isolation is real. |

## If shared infrastructure is the bottleneck

We expect to see:

```
Aggregate throughput at SLO:
  P=1: ~1000 subscribers
  P=2: ~1000-1100 subscribers
  P=4: ~1000-1200 subscribers
  P=8: ~1000-1200 subscribers
```

Key signatures:

| Evidence | What it means |
|----------|---------------|
| Total relay CPU < total available cores | The bottleneck is NOT CPU — it's below the relay (kernel, NIC). |
| UDP drops in `/proc/net/udp` | Shared recv buffer is the ceiling. |
| `perf` shows `sendmsg`/`syscall` saturation | NIC/softirq is the ceiling. |
| P=4 on 16 cores ≈ P=1 on 8 cores | Adding cores doesn't help — truly shared. |

## The decisive comparison

Test **P=4 on 16 cores vs P=1 on 8 cores**:

| Outcome | Interpretation |
|---------|---------------|
| P=4 serves ≥ 2× P=1 | **Process isolation is genuine.** Multiple relays extract more throughput from added cores because isolation reduces cross-process interference (GC, scheduler). |
| P=4 serves about 2× P=1 (≈linear with cores) | **Process isolation helps, but not beyond core scaling.** The value of multi-process is bounded by the number of physical cores. |
| P=4 serves < 2× P-1 | **Diminishing returns.** Either per-process overhead dominates, or a lower-level shared resource (memory bandwidth, NIC) caps throughput regardless of processes. |

---

# Risk factors and mitigations

| Risk | Mitigation |
|------|------------|
| **Load generator saturation.** At high subscriber counts, the co-located load generator (handshake crypto, frame reads on shared cores) becomes the bottleneck — not the relays. | Run publishers and subscribers on a **separate host** when possible. At minimum, pin generator processes to disjoint cores (`taskset`). |
| **Ephemeral port exhaustion.** Each subscriber binds a local port. With P=4 × 2000 subscribers = 8000 ports, the default ephemeral range (32K ports on Linux) is safe, but re-use in quick succession may trigger `TIME_WAIT` collisions. | Set `net.ipv4.ip_local_port_range="10000 65000"`. Add `net.ipv4.tcp_tw_reuse=1` (UDP doesn't use this, but QUIC's connection IDs avoid the issue). |
| **Orphaned processes.** If the driver script is killed mid-run, background relay and loadgen processes are left running. | Use a `trap` EXIT handler that kills all child processes. Use process-group IDs. Write PIDs to a file for manual cleanup. |
| **System-wide GC pauses.** At high RSS, each relay's GC STW could cascade across processes if they share memory bandwidth. | Monitor `go_gc_duration_seconds{quantile="1"}` per relay. If cascading GC is suspected, stagger startup delays. |
| **UDP receive buffer overflow.** Single-socket limit is well-known (baseline.md); multiple processes = multiple sockets, so this *should* improve. But `/proc/net/udp` drops must be checked. | Monitor `netstat -s \| grep -i udp.*drop` or `/proc/net/udp`. |
| **Insufficient total core count to separate relays + load generators.** On an 8-core host, P=4 with 2 cores per relay leaves no room for load generators. | Accept that for P≥4, load-gen cores must overlap relay cores. Flag this in the analysis. The 16-core control test is the clean answer. |

---

# Deliverables

The experiment produces:

1. **`bench-multiproc/run-sweep.sh`** — the orchestrator script
2. **`bench-multiproc/analyze.sh`** — parses results into a summary table
3. **Results JSONL** — one record per (P, N, scenario) cell
4. **Summary table** printed to stdout:

```
P  scenario  N  connected  loss%  p99(ms)  relayCPU  rss(MB)  goros  sustained  reason
1  indep    500   498     0.0      5     0.88       245    3_450    yes
1  indep   1000   985     0.2    227     1.88       981   13_500    yes
1  indep   1500  1370    12.0    345     2.75     2_421   20_100    no      loss+p99
2  indep   1000   990     0.1     14     0.95       502    6_900    yes
2  indep   1500  1480     0.3     42     1.42       995   13_800    yes
2  indep   2000  1950     1.2    185     2.10     1_800   20_400    no      loss>1%
4  indep   1000   992     0.0      8     0.48       260    3_450    yes
4  indep   2000  1980     0.1     22     0.96       520    6_900    yes
4  indep   3000  2950     1.5    285     1.44       780   10_350    no      loss>1%
...
```

---

# Comparison with existing data

The existing `scaling.md` hierarchy study (K=2,4,8 edges with hub) already
shows that hierarchy on one host does NOT multiply capacity — it's a latency
and hub-offload optimization, not a capacity multiplier. That study's most
telling number: at K=8, all 9 relays (1 hub + 8 edges) used only 1.78 of 4
cores and sustained only ~1500 subscribers total.

The *independent*-relay scenario proposed here is cleaner: no hub, no peer
links, no cross-process message overhead. If **even independent relays** on
one host cannot scale aggregate throughput beyond ~1000-1500 subscribers,
then the bottleneck is definitively in the shared infrastructure (kernel
networking, memory bandwidth), not in Go-runtime cross-talk or GC.

If independent relays DO scale, but hubbed relays do not, then the hub's
fan-in/fan-out is the overhead, and the fix is topology-level (distribute
load at the network layer, not via relay-internal peer routing).

---

# Running the experiment

Prerequisites:
- Linux host (WSL works; bare metal preferred).
- Go toolchain (for building the `qumo` binary).
- `taskset`, `curl`, `jq`, `bc` (for parsing/calculations).
- Enough CPU cores: 8+ recommended; 16+ for the control test.

Steps:

```bash
# Build the relay + loadgen
cd /mnt/d/qumo
go build -o bench-multiproc/qumo .

# Generate self-signed dev cert
cd bench-multiproc
cat > gen-cert.sh <<'EOF'
openssl req -x509 -newkey rsa:2048 -keyout key.pem -out cert.pem \
  -days 365 -nodes -subj "/CN=qumo-relay" 2>/dev/null
EOF
bash gen-cert.sh

# Run the full sweep
bash run-sweep.sh
```

---

# Appendix: Decisive experiment on two hosts

The cleanest test separates relay load from load-generator load entirely:

**Host A** (16 cores): runs all relay processes
**Host B** (dedicated): runs all publishers and subscriber load generators

This eliminates the load-generator-is-also-a-bottleneck risk. If Host A shows
P=4 > P=1 throughput, the effect is real. If P=4 ≈ P=1, the bottleneck lives
in Host A's shared kernel or hardware, and no process-level trick will fix it.

The relay processes themselves need no cross-host awareness — each is
independent, listening on its own port, and any remote client can subscribe
to any port. The split-host test is a matter of pointing the load generators
at Host A's IP and different ports.

