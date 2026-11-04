# Sakura evaluation, November 2026: the relay's two cliffs

The qumo-deploy sizing run of 2026-11-04 (one Sakura Cloud relay, 4 cores /
8 GB, qumo 0.18.261031, one publisher to N subscribers) found two cliffs:

1. **Audio** (160 B every 20 ms, one object per stream): fine at 1000
   subscribers (RSS 304 MB), but on the way to 2000 the relay's RSS passed
   4 GB and Nomad's cgroup killed it, twice.
2. **Video** (30 fps, 20–60 KB frames, a group every 2 s, ≈ 10–15 Mbps per
   subscriber): broken at 10 subscribers (6–34 % loss, seconds of latency),
   with the relay at 22–32 % of one core and the node sending 150–180 Mbps.

This page reproduces both on two dedicated Sakura VMs, outside the dev
environment, and attributes them. It is a lab record: two VMs on the same
shared segment, one run per cell unless a table says otherwise.

## Setup

| | perf-relay | perf-load |
|---|---|---|
| Zone, plan | tk1a, 4 cores / 8 GB | tk1a, 8 cores / 16 GB |
| Image | Ubuntu 22.04 cloudimg, kernel 5.15 | same |
| `net.core.{r,w}mem_max` | 212992 (the default, as on the dev relays) unless a section says 16 MiB | 16777216 |
| Runs | `qumo playground --relay-addr 0.0.0.0:4433` (auth off), `RELAY_PPROF=1`, no Nomad, no cgroup limit | qumo-deploy `tools/sakura-eval/loadtest` |

- The relay is built from this branch (`GOOS=linux GOARCH=amd64 CGO_ENABLED=0
  go build -trimpath`); with no new variable set it is v0.18.261031.
- A sampler on the relay VM records every 2 s the relay's RSS and CPU ticks,
  `go_goroutines`, the NIC's bytes, and `/proc/net/snmp` Udp `RcvbufErrors` /
  `SndbufErrors`, and takes `/debug/pprof/heap` every 10 s.
- loadtest runs on perf-load (the "2 VMs" columns) or on perf-relay against
  `127.0.0.1` (the "loopback" columns, the same relay, binary and sysctl, no
  network). Stages: `-warmup 10s -measure 60s` for audio,
  `-warmup 5s -measure 30s` for video.
- loadtest's subscribers grant the relay `MaxIncomingUniStreams = 1<<20`
  (`tools/sakura-eval/loadtest/moq.go`); the section **The client's stream
  limit** measures what that does.

## Audio: the OOM is an unbounded send backlog

### Reproduced (2 VMs, default sysctl)

| Subscribers | Verdict | p50 / p95 / p99 ms | Loss | Relay CPU (1 core = 100 %) | RSS | Udp RcvbufErrors |
|---:|---|---|---:|---:|---:|---:|
| 500 | ok | 3.7 / 7.7 / 13.6 | 0 % | 122 % | 148 MB | +11 K over the stage |
| 1000 | ok | 8.1 / 38.3 / 64.0 | 0 % | 255 % | 280 MB | ~16 K/s |
| 2000 | relay killed | — | 100 % at the probe | 304 % (profile) | 7.8 GB, then OOM kill | 2.96 M total |

At 2000 the RSS grew by about 130 MB/s for a minute, from 0.6 GB to 7.8 GB,
and the kernel killed the process (`Out of memory: Killed process (qumo)
anon-rss:7815080kB`); there is no cgroup here, so it took the whole 8 GB node
rather than dev's 4 GB job.

### Where the memory is

Heap, in use, during the 2000 ramp (3.1 GB sampled; RSS was 7.7 GB):

| # | flat | flat % | Frame |
|---:|---:|---:|---|
| 1 | 1817 MB | 58.1 % | `quic-go/internal/wire.init.0.func1` (the pooled STREAM-frame buffer, packet-sized) |
| 2 | 564 MB | 18.0 % | `quic-go.newSendStream` |
| 3 | 180 MB | 5.8 % | `quic-go.newStreamFlowController` |
| 4 | 142 MB | 4.5 % | `quic-go.init.0.func1` (packet buffers) |
| 5 | 98 MB | 3.1 % | `context.withCancel` (each SendStream's context) |
| 6 | 81 MB | 2.6 % | `quic-go.(*framer).AddActiveStream` |
| 7 | 35 MB | 1.1 % | `quic-go/internal/utils/ringbuffer.grow` (the framer's stream queue) |
| 8 | 31 MB | 1.0 % | `quic-go.(*outgoingStreamsMap).openStream` |
| 9 | 23 MB | 0.7 % | `context.WithCancelCause` |
| 10 | 7 MB | 0.2 % | `gomoqt/moqt.newGroupWriter` |

Frame 1 is reached through `gomoqt/moqt/internal/message.StreamType.Encode`
(61.5 % cum): the first byte written to each new group stream makes quic-go
take a packet-sized frame buffer for it. alloc_space over the same window is
the same story (`newSendStream` 27 %, packet buffers 20 %, STREAM-frame
buffers 10 %, flow controllers 8 %). No relay frame is in either top ten.

So the heap is about a million **opened but unsent group streams**, each
holding a ~1.4 KB buffer for a ~180-byte object, a SendStream, a flow
controller and a context.

### Why they accumulate

- The relay opens one QUIC uni stream per group for every subscriber
  (`deliverGroup` → `OpenGroupAt`). Its only backpressure on that is the
  subscriber's MAX_STREAMS: when the subscriber grants no more streams,
  `OpenGroupAt` blocks, times out after 30 ms and the group is dropped.
- loadtest grants 2^20 streams, so that backpressure never engages.
- At 1000 subscribers the relay already uses 2.5 of its 4 cores; 2000 needs
  more than 4. Then quic-go's per-connection send loops (packing, sealing,
  `sendmsg`) fall behind the egress goroutines, which only open a stream and
  write 180 bytes. The difference queues inside quic-go without a bound.
- The receive side makes it worse: quic-go asks for a 7 MB receive buffer and
  gets 416 KiB under `rmem_max` 212992 (it logs `failed to sufficiently
  increase receive buffer size`), and from 1000 subscribers on the socket
  overflows (~16 K datagrams/s dropped, mostly the subscribers' ACKs). Lost
  ACKs mean retransmissions and smaller congestion windows: more to send, sent
  more slowly.

The cliff is therefore not a per-session memory cost that is nonlinear in
sessions (1000 sessions hold 280 MB); it is a queue whose growth rate is the
gap between open rate and send rate, which becomes positive once the relay is
out of CPU, and nothing bounds it. It is also a resource the client decides:
any client that grants a large MAX_STREAMS can make the relay queue streams
for it.

### The client's stream limit

The same run with loadtest's subscribers granting **100** uni streams (quic-go's
default, and the order of what real clients grant) instead of 2^20; baseline
relay binary, default sysctl, 2 VMs:

| Subscribers | Client MaxIncomingUniStreams | Verdict | p50 / p95 / p99 ms | Loss | Receiving | Relay CPU | Relay RSS (max) |
|---:|---:|---|---|---:|---:|---:|---:|
| 500 | 2^20 | ok | 3.7 / 7.7 / 13.6 | 0 % | 500 | 122 % | 148 MB |
| 500 | 100 | ok | 3.7 / 7.7 / 15.1 | 0 % | 500 | 121 % | 146 MB |
| 1000 | 2^20 | ok | 8.1 / 38.3 / 64.0 | 0 % | 1000 | 255 % | 280 MB |
| 1000 | 100 | ok | 9.0 / 44.1 / 74.3 | 0 % | 1000 | 268 % | 277 MB |
| 2000 | 2^20 | **relay OOM-killed** | — | 100 % | 760 | — | 7.8 GB, growing ~130 MB/s |
| 2000 | 100 | relay-limit | 29 / 561 / 965 | 2.3 % | 1609 | 294 % | **1.05 GB, flat** |

With a realistic grant the relay does not run out of memory at 2000: once the
subscriber's 100 streams are in flight, `OpenGroupAt` blocks, times out after
30 ms and drops the group, which is the designed backpressure. RSS levels off
at 1.0 GB for the whole window and the stage fails on what it should fail on,
CPU (≈ 3 of 4 cores busy, latency in the hundreds of milliseconds, 2.3 % loss).
The receive buffer still overflows (5.8 M `RcvbufErrors` over the run).

So:

- **The dev sizing run's 2000-subscriber OOM is an artifact of the load
  client**, which grants 2^20 streams; with a client-like grant the 4-core
  relay's audio ceiling at 2000 is CPU (latency and loss), not memory. The
  sizing harness should dial with a realistic `MaxIncomingUniStreams`
  (100) so that it measures what real clients see.
- **The relay still trusts the client for its memory.** A client that grants
  a large MAX_STREAMS, by accident or on purpose, gets an unbounded queue in
  the relay once the relay is out of CPU. That is what
  `RELAY_EGRESS_SHED_HEAP_MB` is for (measured below).

CPU over 30 s at the 2000 ramp (304 % of one core): top frames

| # | flat % | cum % | Frame |
|---:|---:|---:|---|
| 1 | 14.5 | 14.5 | `internal/runtime/syscall/linux.Syscall6` (`sendmsg`/`recvmsg`) |
| 2 | 4.6 | 12.8 | `runtime.selectgo` |
| 3 | 2.7 | 2.7 | `runtime.spanClass.sizeclass` |
| 4 | 2.0 | 3.3 | `runtime.sellock` |
| 5 | 1.9 | 2.5 | `runtime.lock2` |
| 6 | 1.7 | 1.7 | `runtime.memmove` |
| 7 | 1.6 | 1.6 | `internal/runtime/maps.ctrlGroup.matchH2` |
| 8 | 1.5 | 1.7 | `runtime.unlock2` |
| 9 | 1.4 | 1.4 | `runtime.(*waitq).dequeue` |
| 10 | 1.3 | 4.8 | `runtime.tryDeferToSpanScan` |

By package (cum, overlapping): quic-go 54 %, gomoqt 26 %, relay 26 %,
syscalls 16 %, GC mark/alloc 13 %.

### Datagram delivery

draft-evens-moq-bench names datagrams for the audio scenario. Neither gomoqt
v0.28.0 nor v0.28.1 (the latest; its Go module is unchanged from v0.28.0) has
a datagram API for objects: `moqt` only sets `EnableDatagrams` in the QUIC
configuration (and the WebSocket qmux carries datagram frames). `qumo
loadgen` has no datagram option either. The datagram-versus-stream ramp
therefore cannot be run; everything here is one stream per object.

## Video: the relay is not the limit, the path is

Same relay, same binary, same 212992 buffers; the only difference between
the two columns is whether the subscribers are across the network or on the
relay's own host.

| Subscribers | 2 VMs: verdict, loss | 2 VMs: p50 / p95 | 2 VMs: relay TX | 2 VMs: relay CPU | Loopback: verdict, loss | Loopback: p50 / p95 / p99 | Loopback: relay CPU |
|---:|---|---|---:|---:|---|---|---:|
| 5 | ok, 0 % | 1.3 / 2.0 ms | 62 Mbps | 7 % | ok, 0 % | 1.05 / 1.6 / 2.0 ms | 6 % |
| 10 | limit, 12.4 % | 3.5 / 6.5 s | 140 Mbps | 31 % | ok, 0 % | 1.3 / 2.2 / 2.9 ms | 8 % |
| 20 | limit, 68.6 % | 12.2 / 18.3 s | 147 Mbps | 35 % | ok, 0 % | 1.9 / 3.4 / 4.9 ms | 13 % |
| 50 | limit, 89.2 % | 7.6 / 14.6 s | 164 Mbps | 41 % | ok, 0 % | 3.6 / 6.6 / 8.8 ms | 25 % |

- Across the network the relay's transmit rate is pinned at 140–165 Mbps
  however many subscribers there are, the shape of the dev run (150–180 Mbps).
  `SndbufErrors` stayed 0 in every stage: the socket never refused a send.
- On loopback the same relay sends 50 subscribers' worth (several hundred
  Mbps) at single-digit milliseconds and a quarter of a core.
- The relay's CPU across the network (31–41 %) is higher than on loopback for
  fewer bytes: that is QUIC loss recovery, retransmissions and ACK
  processing for a path that drops packets, not the cause of the drops.
- CPU at 20 subscribers across the network: 0.27 core over 20 s, `Syscall6`
  28 %, `runtime.futex` 12 %, the scheduler's idle loop 24 % cum. The block
  and mutex profiles were empty: the relay never set their rates (see
  **Contention profiles**).

**Conclusion: the video cliff on dev is the network path between hosts on the
shared segment (≈ 150–165 Mbps here), not the relay's send path.** One UDP
socket, no GSO tuning and the default 212992 `wmem_max` carry 50 subscribers
on loopback. Whether more sockets or a larger `wmem_max` would help across the
network is answered by the 16 MiB section below; the loopback result already
says the relay can produce the bytes.

## Changes on this branch

Both are off unless their variable is set.

| Variable | Commit | What it does |
|---|---|---|
| `RELAY_EGRESS_SHED_HEAP_MB` | `b5925ee` | A 100 ms sampler reads the heap's object bytes. Past the limit every subscriber's egress drops new groups instead of opening streams (reported to the subscriber as timeout drops, as a timed-out `OpenGroupAt` is), and resumes below 80 % of it. Metrics `qumo_relay_egress_shedding`, `qumo_relay_egress_shed_episodes_total`, `qumo_relay_egress_shed_groups_total`. |
| `RELAY_PPROF_BLOCK_RATE`, `RELAY_PPROF_MUTEX_FRACTION` | `0132a68` | With `RELAY_PPROF`, set `runtime.SetBlockProfileRate` / `SetMutexProfileFraction` so `/debug/pprof/block` and `/mutex` record something. |

Their measurements follow as they are taken.

## Reproducing

On the relay VM, from a directory holding the relay binary as `qumo`:

```bash
ulimit -n 1048576
RELAY_PPROF=1 ./qumo playground --relay-addr 0.0.0.0:4433 --ui-addr 127.0.0.1:8080
```

On the load VM (`ulimit -n 1048576`, `rmem_max`/`wmem_max` 16777216), with
qumo-deploy's loadtest built for linux:

```bash
./loadtest -out runs/audio -relay <relay>:4433 -insecure -metrics http://<relay>:4433/metrics \
  -scenario audio -subscribers 500,1000,2000
./loadtest -out runs/video -relay <relay>:4433 -insecure -metrics http://<relay>:4433/metrics \
  -scenario video -subscribers 5,10,20,50 -warmup 5s -measure 30s -stop-on-limit=false
```

Profiles: `curl -o heap.pb.gz http://<relay>:4433/debug/pprof/heap`,
`.../profile?seconds=30`; `go tool pprof -top <binary> <profile>`. The
loopback columns run the second command on the relay VM against
`127.0.0.1:4433`.
