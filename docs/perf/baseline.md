# Baseline

The current performance envelope of one qumo relay node: what workload it
serves, how many subscribers it holds, and where the latency knee sits. This is
the canonical reference; the other reports build on it.

There are **two distinct capacity questions** that must not be conflated:

- **HOLD capacity** — how many idle-ish connections the relay keeps alive
  (1 frame/s, 64 B). Answer: ~13 000 sessions on this host.
- **Active fan-out capacity** — how many subscribers receive a realtime stream
  within a latency SLO (30 fps, 1200 B). Answer: ~1000 subscribers.

The active-fan-out number is the one that matters for live media, and it is far
smaller because every frame must reach every subscriber.

## Workload definition

The audio baseline workload:

| Parameter | Value | Note |
|---|---|---|
| Frame cadence | 30 fps (≈ 33 ms gap) | realtime audio equivalent |
| Frames per group | 1 | one MoQ object per group (one stream-open per frame) |
| Group open rate | 30 groups/s | consequence of 1 frame/group |
| Frame size | 1200 B | conservative; real Opus ≈ 160 B |
| Publisher | paced, not bursted | verified inter-arrival p50 33.7 ms |
| Topology | 1 publisher → 1 relay → N subscribers | single-node |
| SLO | p99 ≤ 300 ms, loss < 1 %, ≥ 0.95·N connected | realtime budget |

This is deliberately **not** the video GOP workload (which groups 30–120 frames
per stream and cuts p99 ~5×). See [workload model](workload-model.md).

## Active fan-out scaling curve

Single relay, in-process bench (8 shared cores), current code, paced 30 fps:

| Subscribers | p50 | p99 | loss % | fps (of 30) | R.wake p50 |
|---|---|---|---|---|---|
| 100 | 0.9 ms | 1.8 ms | 0 | 29.7 | 0.44 ms |
| 500 | 5.1 ms | 16.7 ms | ~0 | 29.9 | 1.20 ms |
| **1000** | 12.0 ms | 56 ms | 2.0 | 28.8 | 3.32 ms |
| **1500** | 30.5 ms | 304 ms | **46** | **16.0** | 9.9 ms |
| 2000 | 28.6 ms | 167 ms | **50** | **14.9** | 8.4 ms |
| 5000 | 222 ms | 1133 ms | **82** | **5.3** | 44.6 ms |

The relay scales linearly to ~1000 subscribers, then hits a sharp knee between
1000 and 1500 where loss explodes and throughput halves. The knee is a positive-
feedback collapse: rising wake latency → missed frame budgets → ring eviction →
loss → more catch-up load.

With the relay **isolated** (Nomad, relay pinned to 4 cores, load generator on
separate cores), the same knee appears while the relay uses only 1.9–2.8 of 4
cores — it never saturates:

| Subscribers | connected | loss % | p99 | relay CPU (of 4) | RSS MB |
|---|---|---|---|---|---|
| 1000 | 992 | 0.2 | 227 ms | **1.88** | 981 |
| 2000 | 1978 | 16 | ≥ 1000 ms | **2.75** | 2421 |

> **Note:** The isolated-relay resource table predates the reusable-openTimeout
> optimization (#348); current code is marginally better. The 1200 B frame size
> overstates real audio (~160 B), so these numbers are conservative.

The mechanism behind the knee — and why it is not CPU, GC, or locks — is in
[bottleneck attribution](bottleneck-attribution.md).

## HOLD capacity (connection holding)

A different regime: minimal work (1 group/s, 64 B), measuring how many sessions
the relay keeps alive. Measured with the out-of-process `qumo loadgen`, relay
pinned via `taskset`, scraped from `/metrics`.

| cores | target subs | connected | CPU %/core | RSS MB | goroutines | GC p99 |
|---|---|---|---|---|---|---|
| 2 | 6000 | 5605 (93 %) | 51 | 1012 | 39 265 | 2.1 ms |
| 4 | 12000 | 11335 (94 %) | 42 | 2320 | 79 401 | 6.7 ms |
| 6 | 16000 | 13352 (83 %) | 30 | 3233 | 93 736 | 4.9 ms |

The sustainable HOLD ceiling is **~13 000 sessions** (establishment peaks ~15 000
then attrits to a stable 13 000). At 13 000 the relay is not bound by anything
it owns: CPU 30 %/core, GC p99 ≤ 6.7 ms, RSS 3.2 GB, and only **11 open file
descriptors** (one UDP socket). The attrition above ~13 000 is external to the
relay.

> **Warning:** The leading hypothesis for the ~13 000 attrition is the **UDP
> receive buffer**: quic-go requests a 7 MB recv buffer, and a small
> `rmem_max` clamps it (at ~15 000 connections the aggregate ACK/keepalive
> traffic could overflow a clamped buffer). Still **unconfirmed** for this
> regime. Corrections (2026-09-26): current WSL reports `rmem_max` = 4 MiB (not
> ~212 992), and privilege **is** available here (`wsl -u root` — raised to
> 16 MiB this cycle). That raise did not change the *chain-harness* K≥2
> collapse (ledger C21 — different mechanism: in-process saturation), but the
> decisive test for the HOLD regime (re-run the out-of-process loadgen sweep
> with 16 MiB buffers and compare the ~13 000 attrition) has **not** been run.
> Do not treat 13 000 as a recv-buffer finding; treat it as "external to the
> relay, mechanism pending."

## CI Linux baseline (bench-relay, 2026-09-26)

Reference numbers for current code (gomoqt v0.20.0, quic-go v0.62.0) from the
on-demand `bench-relay` run
[36226116466](https://github.com/qumo-dev/qumo/actions/runs/36226116466)
(GitHub-hosted `ubuntu-latest` runners — the `bench-relay` workflow sizes the
sweep for 2 cores — UDP buffers raised to 16 MiB; artifacts
`relay-bench-30m` + `relay-loadgen-capacity`). The prior baseline tables above
were measured on the WSL2 dev host; this section anchors the envelope to
GitHub-hosted Linux.

**Multi-hop chain latency** (`BenchmarkRelayChain_Series`, depth 1→8):

| depth | median | p99 |
|---|---|---|
| 1 | 0.81 ms | 1.18 ms |
| 3 | 1.70 ms | 1.81 ms |
| 5 | 2.80 ms | 2.89 ms |
| 8 | 4.07 ms | 4.15 ms |

Linear fit: **0.471 ms/hop (R² = 0.998)**; p99−median shrinks to ≤0.1 ms from
depth 3 onward (depth 1: +0.37 ms, one-hop tail) — no per-hop tail
accumulation across a relay chain.

**30-minute soak**: flat sub-millisecond end-to-end latency across the whole
window (slice medians 0.31–0.32 ms, p99 ≤ 0.42 ms, max 3.3 ms across time
slices) — no drift and no leak signature.

**Out-of-process capacity sweep** (`qumo loadgen`, relay isolated from the
load generator): S = 500/1000/2000 sessions all **HOLDS** — 100 % receiving at
every step, latency p50 11.7 → 23.1 → 46.2 ms, heap 90 → 174 → 333 MB,
~170–183 KB/session.

**In-process fan-out sweep** (same run): K=1/2 clean (419/398 fps, fairness
1.0), knee at K=4 (37.9 % loss), K≥32 ≈ 100 % loss. Per [ledger
C21](optimization-ledger.md) this is the single-process harness artifact
(publisher + origin + K leaf relays + K subscribers in one test process),
**not** a relay property — the out-of-process sweep above is the
decision-grade fanout instrument. Even K=1 saturates at high rates on these
runners (~240–260 fps against 600–900 fps targets — consistent with the
workflow's 2-core sizing); the 4-core WSL dev host held 900 fps/K=1 clean.

## Per-session cost

Consistent across both regimes: **~7 goroutines and ~470 KB RSS per session**,
essentially all quic-go per-connection state and goroutine stacks. The relay
itself spawns zero per-session goroutines. A four-minute soak at 10 000 sessions
showed zero attrition and an RSS that plateaued (Go `MADV_FREE` reaching steady
state, not a leak).

## Capacity model

Where the data supports a fit:

- **Goroutines(S) = 7.0 · S** (R² ≈ 1 across 3.6K–13.4K sessions).
- **RSS(S) ≈ 200–470 KB · S**, per-session-dominated.
- **HOLD ceiling(cores) ≈ min(2.8·cores_K, ~13K)** on a single host — linear to
  ~4 cores, then capped by the co-located load generator.
- **Active-fan-out ceiling ≈ 1000** under the 300 ms SLO, latency-bound.

A combined `Sessions = f(CPU, Mem, fps, size, ...)` surface is **not** fit:
with the relay never the binding constraint at the measured ceilings, there is
no relay-side surface to model. Fitting one would be unfounded.

## Reproduce

```bash
# Cross-compile the integration bench for Linux (Windows is unreliable for
# quic-go stress).
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -tags=integration \
  -o /tmp/relay_bench ./internal/relay/

# Active fan-out sweep (audio baseline).
FANOUT_GAP=33ms FANOUT_FPG=1 FANOUT_KS=1000,1500,2000 BENCH_DURATION=8s \
  /tmp/relay_bench -test.run='^$' -test.bench=FanoutSingleRelay -test.benchtime=1x

# HOLD capacity (out-of-process, true relay resource use).
cd bench-nomad && KLIST=0 NLIST=1000,1500,2000 GPS=30 SIZE=1200 bash run-study.sh
```

## See also

- [Bottleneck attribution](bottleneck-attribution.md) — why the active-fan-out
  knee is where it is.
- [Scaling](scaling.md) — cores and topology.
- [Workload model](workload-model.md) — the 1-frame-per-group choice.
