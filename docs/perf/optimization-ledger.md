# Optimization ledger

The running log of every optimization attempt in gomoqt and qumo, with its
measured result and terminal classification. The frame is falsification: gomoqt
and qumo are assumed near-optimal, and the job is to find evidence of a *missed*
optimization — not to hunt for speculative wins. quic-go, the Go runtime, the OS,
and hardware are out of scope (classified External).

Every candidate ends in exactly one state:

- **Confirmed** — measured, reproducible, statistically significant improvement.
- **No improvement** — ran, delta within the noise floor.
- **Negligible** — real effect but below materiality (quantified).
- **External** — root cause outside gomoqt + qumo.
- **Configuration** — a knob or deployment setting, not code.

Materiality: a change counts only if it moves a relevant metric (per-session CPU,
allocs, live memory, or a micro-bench ns/op or allocs/op) past the noise floor
without an offsetting regression.

## Profile baseline

WSL relay, cores 0–3, GOGC 1000, `RELAY_PPROF`:

| point | sessions | goroutines | heap live | RSS | GC p99 | relay CPU (of 4) |
|---|---|---|---|---|---|---|
| cruise | 7 831 | 54 840 | 1.35 GB | 1.6 GB | 1.5 ms | ~1.28 (32 %) |
| ceiling | ~10 900 | 76 358 | 2.15 GB | 3.0 GB | 2.8 ms | ~1.33 (33 %) |

CPU flat at the ceiling: quic-go `sendmsg`/`Syscall6` ~24 %, runtime scheduler
~15 %, quic-go crypto ~2 %. **Relay + gomoqt combined flat < 1 % of CPU.** Heap
alloc-objects top: quic-go handshake crypto ~35 % (transient).

## Candidate log

| ID | Candidate | Smallest experiment | Result | Classification |
|---|---|---|---|---|
| C1 | `broadcastNotify.notify()` allocates 2 obj/broadcast | `BenchmarkBroadcastNotify_Notify`: 2 allocs/85 ns, per-group not per-sub | <0.01 % of allocs at 11K subs | Negligible |
| C2 | relay per-session live heap shrinkable | heap `inuse_space`: no relay node in top-18; ~95 % is quic-go | relay footprint invisible | Negligible |
| C3 | `deliveryHistogram.Observe` per group | prometheus Observe is alloc-free | 0 allocs | Negligible |
| C4 | `time.Now`/`time.Since` per group | stdlib, alloc-free | 0 allocs | Negligible |
| C5 | relay mutex contention under fan-out | relay locks absent from profile; cost is runtime-internal from parked goroutines, not relay `sync.Mutex`; designs are 1-writer/lock-free | no contended relay lock | Negligible |
| C6 | fill-path per-frame allocs | groupCache is lock-free append-only since #314 | 0 alloc/frame | Negligible |
| C7 | gomoqt subscribe-path allocs | `newGroupWriter` 1.89 %, total gomoqt flat ~2.4 % vs quic-go ~85 % | real but <2.5 %; GC already cheap | Negligible |
| C8 | frame copy in egress | previously falsified (F2 study) | — | No improvement |
| C9 | other per-frame prometheus updates | ingress counter is per-group/publisher, not per-sub; `metricSubscribersActive` per-connect | sub-Hz | Negligible |
| C10 | `addEgress` per-frame atomic | per-session uncontended atomic | no contention to remove | Negligible |
| C15 | egress-goroutine `select` tax (~3–5 % CPU) | `selectgo` 2.89 %, `sellock` 1.58 % — cost of parked goroutines; removing needs eliminating per-sub goroutines (architectural); relay is only 33 % CPU-utilized at ceiling so it does not raise capacity | real cost, irrelevant to capacity | External |
| C16 | per-frame relay/gomoqt cost at realistic fps | profiled 2969 subs @ gps=10 (30K deliveries/s): relay+gomoqt still <1 % CPU, <2.5 % allocs | unchanged mix at 10× fps | Negligible |
| C17 | ingest `trackBuffer.notify()` O(N) per-frame walk (pre-#332 design, never covered by the v0.5.0 ledger) | `BenchmarkTrackBufferNotify` sweep: 54.9 µs/call @1000 subs, 3.4 µs @100 (kept-up); the earlier `len(ch)==0` fast path was measured and rejected (#194: +3–6 % on the empty common path), the structural broadcastNotify redesign was not | writer 54.9 µs → ~0.8 µs @1000 subs (n=6, local); writer path at N≤10 +30–130 ns/frame, +128 B/frame garbage — wins at production fan-out, costs the zero/low-subscriber case (same trade as relay #332/C1) | Confirmed (PR #397, CI run 35532127804: nolisten 72 ns flat @1–1000 vs base 4.18 µs @100 / 43.4 µs @1000, −98 %+; parked @1000 539 ns; Fanout full-push 198 ns @1 → 869 ns @1000; Session_PushVideo @0 subs +24.5 % = +85 ns, 2→4 allocs, the pre-accepted constant; all shared benches `~` — no significant changes) |

## Adversarial audit

Each conclusion was attacked as if it were another engineer's work:

1. *"A structural inefficiency might bloat memory without showing as hot CPU."*
   Live-heap shows relay invisible; the relay spawns **zero** per-session
   goroutines (egress rides gomoqt's `serveTrack` goroutine). No structural bloat.
2. *"GOGC=1000 might hide a relay-controllable GC wall."* GC scans the same heap
   regardless of GOGC; a GOGC 100→800 A/B cut GC CPU 12 %→2 % but moved
   connections ±4 %. GC is not the relay's lever.
3. *"Enterprise auth/metering per-frame cost not profiled."* Metering reports
   every 30 s; auth is at connect — both sub-Hz, off the per-frame path.
4. *"WSL is noisy; real Linux differs."* The bottleneck *class* is portable:
   `sendmsg` is the irreducible egress syscall and the relay is thin over it.
   Real Linux may add GSO — but that is quic-go (External).
5. *"The egress-goroutine `selectgo` tax (C15) matters at the margin."* The
   strongest challenge, and it resolves *for* the conclusion: the relay owns
   none of the goroutines, so the tax is entirely gomoqt+quic-go → External.

No conclusion was overturned; the selectgo-tax challenge moved C15 from
Negligible to External, strengthening the verdict.

## Verdict

No known, measurable, reproducible optimization remains within gomoqt + qumo.
Across the HOLD regime (gps=1, ~11K ceiling) and a realistic-fps regime (gps=10),
relay + gomoqt combined are **< 1 % of CPU** and **< 2.5 % of allocations**; relay
live-heap is invisible. Dominant costs are all External — quic-go (egress
`sendmsg`, handshake crypto, per-connection state) and the Go runtime (scheduling
~7 goroutines/session).

Open follow-ups, all External: distributed load generation (qumo #342) to confirm
the relay's true ceiling; upstream goroutine-count reduction; quic-go GSO /
sendmmsg (no public knob in v0.60).

## See also

- [Bottleneck attribution](bottleneck-attribution.md) — the high-level version
  of this, with the refuted fan-out premise and the gomoqt verdict.
- [Baseline](baseline.md) and [Scaling](scaling.md) for the capacity context.
