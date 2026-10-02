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
affect which peers are dialed (see `PEERS` / `UPSTREAM_ADDR` below):

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
| `PEERS` | (empty) | Comma-separated peer relay addresses (`moqt://host:4433,...`). The node connects to each and relays their announcements. |
| `UPSTREAM_ADDR` | (empty) | Comma-separated upstream relay address(es), e.g. an edge relay's hub(s), or any relay connecting upward in a hierarchy (`role-hub.qumo-relay.service.consul:4433` or a direct `host:port`). Dialed the same way as `PEERS`. |

There is no runtime peer-discovery service — both variables are static,
dialed once at startup and re-dialed with backoff on disconnect. See
[Deployment → Peer topology]({{< relref "deployment/peer-topology" >}}) for
how they fit together, and [Deployment → Nomad]({{< relref "deployment/nomad" >}})
for a worked example of giving relays stable addresses on Nomad.

## mTLS (optional)

| Variable | Default | Description |
|---|---|---|
| `CA_FILE` | (empty) | PEM CA certificate. When set, mutual TLS is enabled between peers. |
| `PEER_CIDRS` | (empty) | Comma-separated networks whose native-QUIC sessions are trusted as relay peers, e.g. a private mesh overlay (`100.64.0.0/10`). When credential auth is on (`QUMO_SIGNING_KEYS_FILE`), a native-QUIC session is a peer only if it comes from one of these networks or presents a client certificate verified against `CA_FILE`; any other native-QUIC session authenticates its announcements like a WebTransport client. Set this (or mTLS) for relay-to-relay and ingress connections. |
| `MTLS_REQUIRED` | `true` | Whether every connection must present a client cert signed by `CA_FILE`. Set to `false` to accept connections without one (verified if presented), e.g. when the relay also serves browser/WebTransport traffic directly. Only applies when `CA_FILE` is set. |

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

## Credential auth & metering (optional)

| Variable | Default | Description |
|---|---|---|
| `QUMO_SIGNING_KEYS_FILE` | (unset) | Path to a JWK Set of the Ed25519 public keys whose credentials the relay admits: the app signing keys registered with qumo. When set, every publisher announcement must carry a credential signed by one of them. A key's `kid` may be omitted; the relay derives it as the key's RFC 7638 thumbprint, the same `kid` qumo assigns, and refuses a file whose `kid` differs. The file is read once at start. Leave unset for open-relay mode. |
| `QUMO_CREDENTIAL_URL` | (unset) | Base URL of the qumo control plane. When set, the relay reports cumulative ingress/egress byte totals per admitted publisher via `POST /v1/usage/events`, keyed by the signing key's `kid`. Requires `QUMO_SIGNING_KEYS_FILE`. |
| `QUMO_RELAY_TOKEN` | (unset) | Shared bearer token the relay presents to the control plane with usage reports. Must match the server's configured token. |

Credentials are app-signed (qumo-deploy ADR 0035) and verified entirely on the relay: an unknown `kid` is refused, then the `EdDSA` signature, `exp` and `iat` (required) and `nbf` (60 s leeway), a lifetime (`exp` − `iat`) of at most one hour, and that `path_auth` covers the announced path. Other claims are ignored.

`QUMO_RELAY_AUDIENCE` and `QUMO_CREDENTIAL_ISSUER` are no longer read. Introspection (`POST /v1/credentials/introspect`) and fetching the control plane's key set are removed.
