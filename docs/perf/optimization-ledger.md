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
| C18 | post-C17, zero-listener notify still pays the close-and-recreate swap: mutex + state + channel allocs per pushed frame with no egress attached | composite publisher pprof (1 kB video + 256 B AAC, zero subs): `broadcastNotify.notify` = 37.9 % of alloc-objects, ~11 % cum CPU; nolisten micro-bench 72 ns/2 allocs | fast path (seq authoritative, listener-count gate, registered per `serve()` lifetime): nolisten 7–11 ns/0 allocs, `Session_PushVideo` @0 subs 4→2 allocs (−128 B/frame), parked/fanout within noise (n=3 local, laptop-noisy) | Confirmed (CI run 36040338257, n=10: nolisten 70.98 → 4.05 ns, −94.3 %, 2 → 0 allocs, 128 → 0 B/op, flat N=1–1000; Session_PushVideo @0 subs 429.6 → 343.7 ns, −20.0 %, 4 → 2 allocs; parked/fanout no regression — several cells improved; flagged codec cells +1.05/+3.40 % have zero code contact,  run-to-run drift) |
| C19 | ingest egress (`trackBuffer.serve`) 1 ms poll-timer fallback fires while the producer is silent — the pathology the relay removed (#"NotifyTimeout removed entirely": ceiling ~4.5 K→10 K+ there) | scan micro-bench: timer arm 62 ns / arm+fire 109 ns / 0 allocs (Go 1.24+ runtime timers); ingest egress runs 1–2 loops per *publisher session* (not per end-subscriber — that is the relay's egress, C15/C16), and post-#397 every `push`/`pushFrames` broadcasts, so the timer only fires when the producer is silent mid-stream | 1000 fires/s × 109 ns ≈ 0.01 % CPU per idle loop, ×1–2 loops per publisher at tens–hundreds of publishers ⇒ orders of magnitude below materiality; removal is a one-line port of the relay's proven change **if** publisher-session counts ever reach ~10⁴ | Negligible |
| C20 | re-validate External entries (C7/C15/C16) against gomoqt v0.20.0 + quic-go v0.62.0 via a full paced relay chain (origin + K leaves, `BenchmarkRelayChain_FanoutSweep`, healthy-run CPU+heap pprof on Windows laptop) | rate-scan first: loss is rate-dependent (K≤4 clean @ ~400 fps, K=8 0.04 %, K=16 0.00 % @ 100 fps) — the K≥8 @ 500 fps "cliff" is the host's Windows loopback per-datagram UDP wall (~2.5–3 K deliveries/s: `execIO`+`cgocall` ≈ 60 % of samples, CPU ≈ 0.8/12 cores), not a relay/gomoqt regression; profile at 100 fps/K=16 (0 % loss) | CPU: Windows UDP transport (`execIO`+`cgocall`) ≈ 30–35 %, runtime sched/wake ≈ 20 %, quic-go self ≈ 8–12 %; gomoqt self (serveTrack cum 5.0 % minus relay 3.9 %) ≈ 1–3 %; **C15 select tax 5.1 % (selectgo 4.23 %+sellock 0.87 %) vs 4.47 % claimed — reproduces**; **C16 relay+gomoqt ≈ 4–5 % of a 61 %-syscall profile — no qumo-owned node >2 %** (largest: egress cum 3.9 %); **C7 newGroupWriter 3.1 % of alloc_space vs 1.89 % claimed — same order, verdict holds** (new known trade: `broadcastNotify.notify` 4.6 % of allocs at ≥1 listener, the C18 fast-path cost, absent at 0 listeners) | Confirmed (local Windows, no CI artifacts; C7/C15/C16 External/Negligible all hold on v0.20.0 — no candidate; Windows-loopback wall is a harness caveat, cross-check on CI bench Linux before using chain loss numbers for anything) |
| C21 | FanoutSweep K≥2 loss cliff — regression on v0.20.0, environment wall, or harness artifact? (follow-up to C20's caveat) | controlled single-host A/B (WSL2, 4C, gomoqt v0.18.0 `367187a` vs v0.20.0 `c5fed55`, same quic-go v0.62.0), K∈{1,2,8}: K=1 clean on both (407 fps, ~0 % loss); K=2 98.2 %→50.0 %, K=8 96.9 %→87.5 % loss — **collapse exists on both gomoqt versions (pre-bump worse)**; structure-dependent not rate-dependent (900 fps/K=1 = 0.2 % vs 100 fps/K=8 = 75.3 %); GOMAXPROCS-insensitive (P=2/4 fail alike); UDP buffers 4→16 MiB: no improvement | in-process harness saturation: publisher + origin + K leaf relays + K subscriber clients in ONE process — the repo already removed such benches for exactly this reason ("measured the test harness rather than the relay", CHANGELOG, in-process capacity benchmarks removed); consistent with CI's availability gates covering Stress + K=1 only; the same bench-relay run's **out-of-process capacity sweep confirms clean delivery on real Linux (S=500/1000/2000: 100 % receiving, verdict HOLDS, lat p50 11.7→46.2 ms)** — decision-grade fanout numbers must come from `qumo loadgen`, not this harness | No improvement (correction to C20: the "Windows loopback wall" is not Windows-specific, not a gomoqt regression, and not UDP-buffer configuration — it is in-process harness saturation at K≥2 on any single host; K=1 cells and `qumo loadgen` remain the decision-grade instruments) |
| C22 | ~13 000-session HOLD attrition caused by a clamped UDP receive buffer? (`rmem_max`=4 MiB clamps quic-go's 7 MB request; the decisive test queued in baseline.md's HOLD warning) | controlled A/B on CI Linux via the new bench-relay `workflow_dispatch` inputs (#412: `capacity_rmem_max`, `capacity_sessions`), same commit `46c4991`, probes S=6000/10000/13000 @ hold=10s: arm A clamped 4 MiB vs arm B raised 16 MiB, n=2 (runs 36312137106/36312143970 then 36315082034/36315089521; arms fingerprinted from each job log's sysctl echo — dispatch order held) | round 1: A 72.0 % vs B 90.1 % connected @S=13000 (Δ+18.1 pp) plus S=6000 verdict flip CANNOT-HOLD→HOLDS — large but n=1; round 2 repeats: A 91.7 % vs B 89.4 % @S=13000 (Δ−2.3 pp), S=6000 no flip (96.9 % vs 96.2 %), both arms CANNOT-HOLD at every probe. The between-arm delta **changes sign** round-to-round, and the clamped arm's own spread (72.0→91.7 %) dwarfs round-2's between-arm delta; arm B is stable (90.1/89.4 % @S=13000), so the round-1 "effect" was a clamped-arm outlier, not a reproducible buffer effect. S=10000 co-limited in both rounds (Δ −0.4/+5.7 pp, direction unstable) | No improvement (inconclusive at n=2: raising `rmem_max` 4→16 MiB not shown to move the HOLD attrition; keep baseline.md's "mechanism pending" framing. Operational finding: single-host A/B probes at S≥6000 carry ~20 pp round-to-run spread — future capacity A/Bs need more repeats or distributed load generation (qumo #342); #412's inputs stay merge-worthy for exactly that) |
| C23 | What is the relay's true standalone HOLD ceiling once the load generator stops competing for the same cores? (ledger follow-up; local stand-in for qumo #342's distributed run) | two-process split on one laptop, zero code changes: relay alone inside the WSL2 VM (4 cores, exclusive), `tools/capacity` + `qumo loadgen publish/subscribe` cross-compiled to Windows (12 logical cores), mirrored-networking localhost as the QUIC path, `--insecure` + shared self-signed cert, `RELAY_PPROF=1`; probes S=500–13000 @ hold=10s (S=6000 also @60s), fresh relay per probe, run at main `b7d7c55` | S=500 500/500 HOLDS (one cold first run at 96.4 % = fresh-relay burst artifact, clean on warm repeat); S=6000 90.2 %, S=8000 85.6 %, S=10000 77.6 %/69.8 % (~8 pp burst spread, cf. C22), S=13000 43.0 % with **receiving < connected for the first time** — no ceiling above CI's was found because the offered burst itself saturates the venue: during establishment the relay runs 250–320 % CPU + 32–45 % softirq (VM network stack) while the Windows subscribers burn ~5 cores of handshake crypto; post-burst steady hold is cheap (S≈5400 established: 0.66–0.73 core, goros 7.0–7.1/session, RSS 130–160 KB/session, receiving==connected in every probe). ~25K stays unmeasured: the laptop cannot offer enough concurrent handshakes | External (partial #342 progress: the split venue separates *holding* cost from *offering* capacity and confirms both — steady-state holding ~0.7 core @ 5.4K sessions and stable sessions; but establishment bursts saturate a single laptop well below CI's reach, so verdicts there are burst-dominated; the real ceiling answer still needs dedicated hosts per #342) |
| C24 | Does gomoqt own a material share of the establishment-burst CPU — the saturated regime no prior profile covered (C16/C17/C18/C20 were all steady-state)? (follow-up to C23's External classification) | 20s pprof CPU profile captured *during* the S=8000 establishment burst (the C23 split venue, fresh relay, main `4408e72`, gomoqt v0.20.0 clean module build — no replace directives): 28.58s samples @ 142.9% avg relay CPU, mid-burst snapshot 244% CPU + 46% softirq; composition via `pprof -focus` per package | quic-go ≈ 38 % cum (handshake machinery, packet pack/send), crypto ≈ 28 % (TLS 1.3 per-handshake signing — cert is ECDSA P-256, `tools/capacity/cert.go`, so the `bigmod.addMulVVW1024` 14.9 % top node is protocol cost, not harness distortion), runtime+syscalls ≈ 39 % (sched/futex/`Syscall6` network I/O), **gomoqt self ≈ 6.75 % and diffuse**: largest single region `TrackWriter.openGroupWithSequence` 3.11 % cum (per-session group-open fanout via `chansend`), everything else < 0.25 %. Burst failure mode is dominated by kernel softirq (32–45 %, off-process), handshake crypto, and client-side crypto (~5 Windows cores) — none reachable from gomoqt; Amdahl ceiling for a zero-cost gomoqt < 7 % of relay burst CPU, far below what could move the connected% curve | No improvement (no gomoqt-owned bottleneck at the burst phase: materiality gate δ=5 % fails on the largest region — no code change justified. Honesty note: n=1 profile; for the verdict to flip the top region would need >60 % run-to-run fluctuation, unprecedented in C16–C20 profiles. Relay-side burst headroom, if any, lives in quic-go/crypto/kernel, not gomoqt; the true establishment ceiling still needs dedicated hosts per #342) |

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
the relay's true ceiling; upstream goroutine-count reduction. (Resolved: quic-go
GSO — v0.62.0 enables GSO **automatically on Linux**, kernel-detected with a
per-remote fallback and `QUIC_GO_DISABLE_GSO` as kill-switch; there was never a
Config knob to wait for. Windows has no GSO path; the remaining headroom there
is sendmmsg-style batching, an upstream quic-go matter.)

## See also

- [Bottleneck attribution](bottleneck-attribution.md) — the high-level version
  of this, with the refuted fan-out premise and the gomoqt verdict.
- [Baseline](baseline.md) and [Scaling](scaling.md) for the capacity context.
