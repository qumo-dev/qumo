---
title: Configuration
description: Environment variables and flags for configuring the qumo relay.
weight: 2
---

Apart from `--role`, qumo is configured entirely through **environment
variables** — there is no config file. This page groups every variable by
concern.

Set them however your platform prefers — inline, an env file, systemd's
`EnvironmentFile=`, or Docker's `--env-file`:

```bash
RELAY_NAME=relay-tokyo qumo relay          # inline
set -a && . ./relay.env && set +a          # from a file you wrote
qumo relay
```

## Server

| Variable | Default | Description |
|---|---|---|
| `RELAY_ADDR` | `:4433` | Bind address (QUIC/MoQT). Dual-stack — binds both IPv4 and IPv6, so `localhost` works on hosts where it resolves to `::1` (e.g. Windows). Also serves HTTP health/metrics on the same port. |
| `CERT_FILE` / `KEY_FILE` | `certs/server.crt` / `certs/server.key` | TLS certificate and key. Required — the relay exits at startup if it can't load them. |

The node's **topology role** is a CLI flag, not an env var. It is an
operator-facing label logged at startup for visibility only — it does not
affect which peers are dialed (see `PEERS` below):

```bash
qumo relay --role hub    # or "edge"; omit for a standalone / flat relay
```

## Node identity

| Variable | Default | Description |
|---|---|---|
| `RELAY_NAME` | `relay-<hostname>` | Human-readable node identifier. |

## Static peers

| Variable | Default | Description |
|---|---|---|
| `PEERS` | (empty) | Comma-separated relays to dial, as `host:4433`. Each host is resolved to all its addresses and every address is dialed, so a DNS name for a group of relays (`role-hub.qumo-relay.service.consul:4433`) connects to each. The node relays their announcements. |

There is no runtime peer-discovery service — the list is static, dialed once
at startup and re-dialed with backoff on disconnect. See
[Deployment → Peer topology]({{< relref "deployment/peer-topology" >}}) for
how they fit together, and [Deployment → Nomad]({{< relref "deployment/nomad" >}})
for a worked example of giving relays stable addresses on Nomad.

## Peer trust (optional)

| Variable | Default | Description |
|---|---|---|
| `CA_FILE` | (empty) | PEM CA certificate. A session whose client certificate it verifies is a **trusted relay peer**: it is never asked by the auth server. Client certificates stay optional for everyone else (browsers present none). Relays this one dials are verified against the system roots plus this CA, and this relay presents its `CERT_FILE` as its client certificate. Unset: no session is a peer. |

See [Deployment → TLS & mTLS]({{< relref "deployment/tls" >}}).

## Graceful migration / GOAWAY (optional)

| Variable | Default | Description |
|---|---|---|
| `GOAWAY_REDIRECT_URI` | (empty) | Escape-hatch mobility primitive: on shutdown, redirect clients/peers to a successor relay. Route/subscription migration (make-before-break) is the primary mechanism — set this only when you also want graceful-shutdown redirects. |

## Capacity

| Variable | Default | Description |
|---|---|---|
| `GROUP_CACHE_SIZE` | `8` | Completed groups each track's ring retains for late/backfill subscribers. Raise to absorb longer subscriber startup lag, at the cost of per-track memory. |
| `FRAME_CAPACITY` | `1500` | Frame buffer capacity in bytes (roughly one network MTU). |
| `RELAY_UDP_RCVBUF` | `262144` | UDP receive buffer (`SO_RCVBUF`) for the relay's QUIC listener socket. Raise on deployments pushing beyond ~15K concurrent sessions in burst. Set to `0` to use the unmodified OS default. |
| `RELAY_GOGC` | (unset) | GC target percentage. Opt-in: if unset (and `GOGC` is unset), the Go runtime default (100) is used. A fan-out relay's goroutine stacks dominate RSS, so GC-scan CPU grows with session count; `RELAY_GOGC=600..1600` reached ~18–20K sessions on an 8-core host. `GOGC` (the runtime's own env var) always wins if set. Do **not** use `GOMEMLIMIT` for this workload — it forces constant GC and collapses throughput. |

Run `qumo doctor` to see the effective GC target, which input won, and why —
read-only, it changes nothing. See [Observability]({{< relref "observability" >}}).

## Profiling

| Variable | Default | Description |
|---|---|---|
| `RELAY_PPROF` | (unset) | Opt-in `net/http/pprof` endpoints (`/debug/pprof/...`) alongside `/metrics`. Off by default — enable only on a trusted/loopback interface. |

{{< callout type="warning" >}}
`RELAY_PPROF` is checked for **emptiness, not truthiness**: any non-empty value
enables pprof, including `RELAY_PPROF=0` and `RELAY_PPROF=false`. To keep it
off, leave the variable **unset** — do not set it to `0`, which switches
profiling *on* and exposes heap object graphs and goroutine stacks on the
relay's HTTP port.
{{< /callout >}}

## CORS — WebTransport origin check

| Variable | Default | Description |
|---|---|---|
| `CORS_ALLOWED_ORIGINS` | (unset) | Origins permitted to open WebTransport sessions to `qumo relay`, `qumo rtmp`, and `qumo rtsp`/`rtsp-push`. Comma-separated. `*` allows any origin; `same-host` allows any port on the request's own host. If unset, only same-origin and headerless (non-browser) clients are accepted. |

## Session auth (required)

A relay admits each client session in exactly one of two ways. Setting both, or neither, stops the relay at startup.

| Variable | Default | Description |
|---|---|---|
| `QUMO_AUTH_URL` | (unset) | An auth server the relay asks when each client session connects. `https://`, or `http://` on a loopback host only. |
| `QUMO_AUTH_PUBLIC` | (unset) | Comma-separated subtree patterns (`anon/**`, `demo/**`) that any session may publish and subscribe to, with no server. `**` opens everything; use it for development only. |

**Trusted peers are never asked:** sessions with a client certificate verified against `CA_FILE`, and peers this relay dials (`PEERS`).

### The auth server contract
A subset of [`moq-auth`](https://github.com/kixelated/moq/blob/main/doc/bin/relay/auth.md) (qumo-deploy ADR 0035). When a client connects, the relay POSTs JSON:

```json
{"id": "<random hex>", "event": "connect", "node": "<RELAY_NAME>", "transport": "webtransport|quic",
 "remote": "ip:port", "local": "ip:port", "server_name": "relay.example.com",
 "path": "/as/dialed", "query": "jwt=<credential>"}
```

**The relay never parses the credential;** the client puts it in the connect URL (`https://relay.example.com/…?jwt=…`, or `moqt://relay.example.com/…?jwt=…` over native QUIC) and the relay forwards `query` as is.

The server answers with a grant:

```json
{"publish": ["acme/app/**"], "subscribe": ["acme/**"], "expires": 1767225600, "revalidate": 30}
```

- **Patterns** are subtree patterns only: `**` (everything) or `a/b/**` (`a/b` and everything beneath it, on `/` boundaries).
- **Unsupported fields** (`root`, `mounts`, `peer`) make a grant invalid. `tier` is ignored.
- **Admission:**
  - A 2xx with a non-empty grant admits the session.
  - A 401 or 403 refuses it. A WebTransport client gets that status before the upgrade; a native-QUIC session is closed with `0x2` (Unauthorized).
  - Anything else refuses too: a timeout, a 5xx, an invalid grant, or a grant that names nothing. Nothing is admitted while the auth server is down; a WebTransport client gets 503.
- **Announcements** are routed only if the grant's `publish` patterns cover their path.
- **Subscriptions** are served only if the grant's `subscribe` patterns cover their path. A refused subscription gets the same answer as a path that doesn't exist (`NotFound`), so a client can't probe for paths it may not see.
- **Trusted peers**, and peers this relay dials, are never checked.
- **Not yet enforced:**
  - which announcements a session can list: announce interest still lists every path under the requested prefix (qumo-dev/qumo#418);
  - `revalidate` (#419) and `expires` (#423).

With `QUMO_AUTH_PUBLIC`, a session that presents a credential (`?jwt=`) is refused rather than admitted on the public grant.

Metrics: `qumo_relay_auth_requests_total{event,result}` (`admitted`, `refused`, `invalid`, `error`), `qumo_relay_announcements_refused_total` and `qumo_relay_subscribe_authorizations_total{result}` (`admitted`, `not_covered`).
