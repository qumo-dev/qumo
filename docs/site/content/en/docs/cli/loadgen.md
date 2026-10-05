---
title: loadgen
description: Drive an out-of-process capacity load against a running relay.
weight: 8
---

Out-of-process capacity primitives against a running qumo relay. Both
subcommands are pure remote clients — they dial `--relay`, trusting `--ca` (or `--insecure` for a self-signed dev relay)
and never spawn a relay themselves. This matters: running load clients and
the relay in one process makes client-side QUIC-handshake CPU, not the relay,
the bottleneck.

## Usage

```
qumo loadgen <subcommand> [flags]
```

| Subcommand | Description |
|---|---|
| `publish` | Publish one trickle track to the relay (keep running during a run). |
| `subscribe <N>` | Launch N subscriber sessions and measure the relay's hold + per-session cost. |

### publish

| Flag | Default | Description |
|---|---|---|
| `--ca <file>` | (required unless `--insecure`) | PEM file of the relay's TLS cert/CA to trust. |
| `--insecure` | `false` | Skip relay TLS verification (dev; self-signed relay). |
| `--relay <host:port>` | `127.0.0.1:4433` | Relay MoQT address to dial, or a `moqt://host:port/path?jwt=…` URL to connect with a credential to a relay with auth on. Only the `host:port` is logged. |
| `--path <path>` | `/bench/carry` | Broadcast path. |
| `--track <name>` | `data` | Track name. |
| `--gps <float>` | `0.5` | Groups per second (trickle rate). |
| `--size <bytes>` | `64` | Frame size in bytes (min 16). |
| `--metrics <url>` | `http://<relay>/metrics` | Relay `/metrics` URL. |
| `--keepalive <dur>` | `5s` | QUIC keep-alive period. |
| `--idle-timeout <dur>` | `30s` | QUIC max idle timeout. |

### subscribe

| Flag | Default | Description |
|---|---|---|
| `--ca <file>` | (required unless `--insecure`) | PEM file of the relay's TLS cert/CA to trust. |
| `--insecure` | `false` | Skip relay TLS verification (dev; self-signed relay). |
| `--relay <host:port>` | `127.0.0.1:4433` | Relay MoQT address to dial, or a `moqt://host:port/path?jwt=…` URL to connect with a credential to a relay with auth on. Only the `host:port` is logged. |
| `--path <path>` | `/bench/carry` | Broadcast path. |
| `--track <name>` | `data` | Track name. |
| `--hold <dur>` | `30s` | How long to hold sessions after establishment. |
| `--results <dir>` | (optional) | Directory to append a capacity JSONL record to. |
| `--metrics <url>` | `http://<relay>/metrics` | Relay `/metrics` URL. |
| `--keepalive <dur>` | `5s` | QUIC keep-alive period. |
| `--idle-timeout <dur>` | `30s` | QUIC max idle timeout. |

## Example

Run the publisher and the subscriber load against a relay running elsewhere —
the publisher stays up for the duration of the run:

```console
$ qumo loadgen publish --relay 127.0.0.1:4443 --ca certs/server.crt
INFO loadgen publishing relay=127.0.0.1:4443 path=/bench/carry gps=0.5 size=64
```

Then, in another shell, launch the subscriber load. It reports the relay's
own per-session cost, scraped from its `/metrics` before and after:

```console
$ qumo loadgen subscribe --relay 127.0.0.1:4443 --ca certs/server.crt --hold 5s 50
loadgen subscribe → relay 127.0.0.1:4443 (path /bench/carry)
  offered sessions : 50
  connected        : 50
  receiving        : 50
  relay Δgoroutines: 384 (7.7/session)
  relay ΔRSS       : 13.1 MB (267.8 KB/session)
  relay sessions   : 51 active (relay-reported)
  e2e latency      : p50 1.1ms  p95 2.3ms  p99 2.6ms  (100 samples)
  verdict          : HOLDS
```

`verdict: HOLDS` means the relay carried every offered session for the full
hold; raise `N` until it stops holding to find the ceiling.

`subscribe --results <dir>` appends a machine-readable capacity record to a
JSONL file — useful input for your own sweep/reporting tooling if you're
scripting a series of runs at increasing `N` to find a relay's session
ceiling.

## Configuration

No environment variables — the flags above are the entire surface. The
relay it measures is configured separately.

## See also

- [Observability]({{< relref "../observability" >}}) — the metrics that back the numbers `loadgen` reports.
- [relay]({{< relref "relay" >}}) — the server you point it at.
