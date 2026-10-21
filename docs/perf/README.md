# Performance

This directory documents the performance characteristics of the qumo relay and
its underlying MoQ library, gomoqt. The work is a measurement-first performance
engineering cycle: every conclusion is backed by a benchmark or profile, and
optimizations are only adopted when they move a measured metric past the noise
floor.

If you are looking for one number: **a single qumo relay sustains roughly 1000
active audio subscribers (30 fps, 1200 B) within a 300 ms p99 SLO**, and the
limiting factor is the per-subscriber fan-out drain, not CPU, GC, or the relay's
own code. The detail and the evidence are below.

## The headline answers

- **Single relay, active fan-out:** ~1000 subscribers at 30 fps / 1200 B, p99
  ~56 ms, < 2 % loss. A sharp latency knee follows at 1000–1500.
- **What limits it:** the per-frame fan-out drain — one published frame must be
  written to every subscriber's QUIC stream, and the tail grows with subscriber
  count. It is a *service-rate* effect, not lock contention or GC.
- **What does *not* limit it:** relay CPU (the relay never saturates its cores),
  GC (~2 %), relay locks (~0), the ring/cache, gomoqt's encode path, or the
  broadcast/notify primitive itself (0.05 % of CPU).
- **Does scaling help?** Cores plateau at ~4–8 (2→4 is transformative, 8→16 is
  nothing). Hierarchy (hub → K edges) cuts p99 ~7× (227 → 34 ms at 1000 subs)
  by dividing the fan-out, but on one host it multiplies capacity by ~1.5×, not
  K× — every relay shares the same cores. Real horizontal capacity needs a relay
  per host.
- **Is gomoqt the bottleneck?** No. In a pure-gomoqt real-QUIC profile gomoqt's
  own code is ~3–4 % of CPU, its egress path is 0-allocation/0-copy, and its
  internal blocking is effectively zero. The remaining latency is owned by
  quic-go, the Go runtime, and the kernel.

> **Warning:** These are single-host lab measurements (WSL2 / loopback, load
> generator co-located). They are shape-valid for attributing bottlenecks and
> ranking levers; they are **not** production capacity numbers. The two blocked
> experiments — raising the UDP recv buffer on bare metal, and distributed
> (multi-host) load generation — are what would turn them into production
> figures.

## Reading order

1. [**Baseline**](baseline.md) — the workload definition, the single-relay
   capacity envelope, and the audio SLO knee. Start here.
2. [**Bottleneck attribution**](bottleneck-attribution.md) — where CPU, latency,
   and waiting actually accumulate; the per-stage decomposition; the fan-out
   optimization candidates that were tried and refuted; the gomoqt verdict.
3. [**Scaling**](scaling.md) — subscriber, core, and topology (hierarchy)
   scaling, and why a single host cannot show real horizontal capacity.
4. [**Workload model**](workload-model.md) — why the audio baseline uses one
   frame per group, how that differs from video GOPs, and the publisher pacing
   check.
5. [**Optimization ledger**](optimization-ledger.md) — the running log of every
   optimization attempt, with its measured result and adopt/reject decision.

## Method

The cycle follows a strict discipline: frame the question, capture a baseline
with its variance, profile under a representative workload, pre-register a
falsifiable hypothesis, make one isolated change, measure before/after, and
revert anything that does not move the metric past the noise floor. "No change"
is an acceptable and common outcome. The benchmark harnesses and profiling
commands are documented in each report.

## Benchmarking & capacity

The relay's benchmarks emit JSONL that a zero-dependency Deno tool turns into a
single, self-contained **dashboard** — open one file, no server:

```bash
# After a bench run has written results.jsonl into <dir> (BENCH_RESULTS_DIR):
deno run --allow-read=<dir> --allow-write=<dir> scripts/relay_bench_report.ts <dir>
# → <dir>/index.html : capacity headline + decision summary + every plot inline,
#   plus the paramexp GP/ML findings when passed --paramexp <report-dir>.
```

**Concurrent-session capacity is measured out-of-process.** Running the relay
and the load clients in one process makes client-side QUIC-handshake CPU — not
the relay — the bottleneck, so the load runs against a *separately running*
relay and the measurement reports the **relay's own** per-session cost by
scraping its `/metrics` (`go_goroutines`, `process_resident_memory_bytes`,
`qumo_relay_sessions_active`) before/after the run.

The `qumo loadgen` CLI is two small primitives — pure remote clients that dial
the relay you point them at and never spawn one:

```bash
qumo loadgen publish       --relay <host:4433> --ca <cert.pem>              # trickle source
qumo loadgen subscribe --relay <host:4433> --ca <cert.pem> --hold 15s 12000 # measure N=12000
```

`subscribe --results <dir>` appends a `capacity`-group record to
`results.jsonl`, which the dashboard renders.

**Sweeping / finding the ceiling** is orchestration, so it lives in a separate
driver — `tools/capacity` — that composes the primitives (starts a relay +
publisher, then probes session counts). Build it with `go build -o capacity
./tools/capacity`. It runs two ways:

- **One box:** `--start-relay` spawns a local relay (self-signed cert generated
  in-process — no `openssl`) pinned via `--relay-cores` so its CPU is isolated
  from the load — a single-box stand-in for two hosts. A **fresh relay is
  started per probe**, so each measurement is independent and never inherits
  residual state from a prior overload. `--sessions` probes an explicit list;
  `--auto` climbs until the relay can't hold to find the ceiling (`--bisect`
  pins the boundary). This is what the `capacity-sweep` job in
  `.github/workflows/bench-relay.yml` runs:

  ```bash
  ./capacity --start-relay --relay-cores 0-1 --sessions "500 1000 2000" --hold 10s
  ./capacity --start-relay --relay-cores 0-1 --auto --start 2000 --max 50000 --bisect
  ```

- **Two hosts:** point it at a relay running elsewhere; it only generates load:

  ```bash
  ./capacity --relay relay.example.net:4433 --ca cert.pem --auto --start 5000 --max 30000
  ```

Every probe appends to `results.jsonl`, so a run renders in the same dashboard.
A distributed (multi-machine) run is what a 25K-session ceiling claim needs to
be *confirmed* rather than extrapolated.

## See also

- Repository benchmarks: `internal/relay/benchmarks_test.go` and `internal/relay/*_bench_test.go` (relay),
  `BenchmarkRelayChain_FanoutSingleRelay` is the canonical fan-out benchmark.
- gomoqt benchmarks: `d:/gomoqt/moqt/*_benchmark_test.go`.
- Distributed load generation: tracked as qumo issue #342.
