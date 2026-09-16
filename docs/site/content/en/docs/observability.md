---
title: Observability
description: Health checks, Prometheus metrics, pprof, and the qumo doctor command.
weight: 4
---

The relay's HTTP port (same port as QUIC/MoQT — `RELAY_ADDR`, default
`:4433`) serves these endpoints alongside the MoQT WebTransport handler:

| Path | Purpose |
|---|---|
| `/health` | Health/status probe |
| `/routes` | Current broadcast paths and selected upstream route details |
| `/metrics` | Prometheus metrics |
| `/debug/pprof/*` | Runtime profiling (opt-in via `RELAY_PPROF=1`) |

```bash
curl http://localhost:4433/health
curl http://localhost:4433/routes
curl http://localhost:4433/metrics
```

`/routes` is a point-in-time JSON snapshot for incident investigation. It
reports each active broadcast path, its selected upstream source, hop count,
upstream RTT estimate, estimated bitrate, and route update time. It is
intentionally separate from `/metrics`: Prometheus remains the source for
time-series dashboards and alerts, while `/routes` exposes the current
path-to-route mapping.

## Prometheus metrics

All metrics are under the `qumo_relay_` prefix.

### Sessions & connections

| Metric | Type | Description |
|---|---|---|
| `qumo_relay_sessions_active` | Gauge | Current number of active MoQT relay sessions. |
| `qumo_relay_subscribers_active` | Gauge | Current number of active MoQT track subscribers. |
| `qumo_relay_session_rtt_ms{remote}` | Gauge | Smoothed RTT to each MoQT session, in ms. |
| `qumo_relay_session_rtt_seconds{remote}` | Histogram | Distribution of session RTT. |
| `qumo_relay_session_estimated_bitrate_bps{remote}` | Gauge | Estimated available bandwidth per session. |
| `qumo_relay_conn_smoothed_rtt_ms{remote}` | Gauge | QUIC-layer smoothed RTT (native QUIC connections only; WebTransport connections are skipped since the transport doesn't expose `ConnectionStats()`). |
| `qumo_relay_conn_packet_loss_rate{remote}` | Gauge | Cumulative packet loss rate (lost/sent) for native QUIC connections. |

### Peer mesh

| Metric | Type | Description |
|---|---|---|
| `qumo_relay_peers_connected` | Gauge | Current number of outbound relay peer connections. |
| `qumo_relay_peer_dial_attempts_total{peer,result}` | Counter | Outbound peer dial attempts (`result` = `ok`/`error`). |
| `qumo_relay_dial_retries_total{peer}` | Counter | Outbound peer dial retries after a failure. |
| `qumo_relay_peer_goaway_received_total{redirect}` | Counter | GOAWAY messages received from upstream peers, by whether a redirect URI was supplied. |

### Routes & broadcasts

| Metric | Type | Description |
|---|---|---|
| `qumo_relay_broadcasts_active` | Gauge | Current number of active relay broadcast routes. |
| `qumo_relay_route_replacements_total` | Counter | Existing routes replaced by a strictly better candidate. |
| `qumo_relay_route_rejections_total{reason}` | Counter | Route candidates that did not displace the existing route, by reason. Includes candidates that were better but not by the hysteresis margin — see [Peer topology]({{< relref "deployment/peer-topology" >}}#route-election). |
| `qumo_relay_routes_retained` | Gauge | Route-election losers currently held as alternates, pending promotion. |
| `qumo_relay_route_promotions_total` | Counter | Retained alternates promoted to active after the incumbent's announcement ended. |

### Traffic & buffers

| Metric | Type | Description |
|---|---|---|
| `qumo_relay_ingress_bytes_total{track}` | Counter | Bytes received from publishers. |
| `qumo_relay_egress_bytes_total{track}` | Counter | Bytes sent to subscribers, including fan-out. |
| `qumo_relay_buffer_depth_groups{track}` | Gauge | Groups currently held in the track's ring buffer. |
| `qumo_relay_group_fills_inflight` | Gauge | Fill goroutines currently running across all track distributors. |
| `qumo_relay_group_delivery_seconds{track}` | Histogram | Time to deliver a complete group to a subscriber. |
| `qumo_relay_subscriber_skips_total` | Counter | Subscribers skipped forward after falling behind the ring buffer. |
| `qumo_relay_subscribe_errors_total{code}` | Counter | Failed subscription requests, by error code. |

## `qumo doctor`

Read-only: explains the relay's *effective* runtime configuration, and why —
it changes nothing.

```bash
qumo doctor
```

```
qumo doctor — effective runtime configuration

GC target (garbage collector)
  Inputs:
    GOGC        = (unset)
    RELAY_GOGC  = (unset)
    GOMEMLIMIT  = (unset)
  Effective:    100%  (source: runtime default)
  Why:          neither GOGC nor RELAY_GOGC is set; the relay leaves the runtime
                default (100) in place. Set RELAY_GOGC on high-fan-out hosts to
                lift the session ceiling.
  Guidance:     A fan-out relay's goroutine stacks dominate RSS, so GC-scan CPU
                grows with session count and becomes the ceiling. On big-memory
                hosts pushing >15K sessions, set RELAY_GOGC (600-1600 reached
                ~18-20K on an 8-core host). GOGC always overrides. Do not set
                GOMEMLIMIT for this workload.
```

See [Configuration → Capacity]({{< relref "configuration" >}}#capacity) for
the underlying variables.

## pprof

Opt-in via `RELAY_PPROF=1`, exposing `net/http/pprof` at `/debug/pprof/`. Off
by default — pprof exposes runtime internals (heap object graphs, goroutine
stacks), so enable it only on a trusted/loopback interface.

```bash
RELAY_PPROF=1 qumo relay
go tool pprof http://localhost:4433/debug/pprof/profile?seconds=30
go tool pprof http://localhost:4433/debug/pprof/heap
```
