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
A relay trusts signing keys from exactly one source, or runs open:

| Variable | Default | Description |
|---|---|---|
| `QUMO_SIGNING_KEYS_FILE` | (unset) | **Self-hosted relay.** Path to a JWK Set of the Ed25519 public keys whose credentials the relay admits: the app signing keys registered with qumo. Every publisher announcement must carry a credential signed by one of them. A key's `kid` may be omitted; the relay derives it as the key's RFC 7638 thumbprint, the same `kid` qumo assigns, and refuses a file whose `kid` differs. The file is read once at start; the relay never calls qumo. |
| `QUMO_CREDENTIAL_URL` | (unset) | **Managed relay.** Base URL of the qumo control plane. The relay polls the trust snapshot (`GET /v1/relays/trust`) every 30 s and reports cumulative ingress/egress byte totals per admitted publisher via `POST /v1/usage/events`, keyed by the signing key's `kid`. Cannot be combined with `QUMO_SIGNING_KEYS_FILE`. |
| `QUMO_RELAY_TOKEN` | (unset) | Bearer token the relay presents to the control plane. Required with `QUMO_CREDENTIAL_URL`. |

Setting neither leaves the relay open.

On a managed relay:
- **No session is admitted until the first snapshot loads.** Only `active` keys admit new sessions, and only while their project isn't suspended.
- **Snapshot changes apply to live sessions within one poll.** A publisher whose key is revoked or removed, or whose project is suspended, has its session closed with MoQ error code `0x2` (Unauthorized) and the reason `key_revoked` or `project_suspended`. A `retired` key admits no new sessions, but its live sessions continue.
- **Fail-static:** if polls fail, the last snapshot keeps answering. After 6 h without a successful poll, new sessions are refused; live sessions continue.
- **Usage and session events** go to `POST /v1/usage/events` every 30 s, as a JSON array of `{type, session_id, role, kid, project_id, metrics?, reason?, ts}`:
  - **`usage`:** a publisher broadcast's cumulative `gateway.ingress_bytes` and `gateway.egress_bytes`. Egress to subscribers is counted on the publisher's broadcast; prefix confinement makes a subscriber's project the path's project.
  - **`session_open` / `session_close`:** one pair per admitted publisher broadcast and per subscriber session credential. The control plane counts live sessions per project from these.
  - **`reason`** (on close): `closed`, `credential_expired`, `key_revoked` or `project_suspended`.
  - **`kid`** is the session's current key, which can change at a refresh. **`project_id`** can't.
  - Session events that fail to send are retried with the next report; at most 10,000 are queued.
- **Metrics:** `qumo_relay_trust_last_success_seconds` (Unix time of the last successful poll), `qumo_relay_trust_poll_failures_total`, `qumo_relay_trust_keys`, `qumo_relay_sessions_ended_total{reason}`.

Credentials are app-signed (qumo-deploy ADR 0035) and verified entirely on the relay: an unknown `kid` is refused, then the `EdDSA` signature, `exp` and `iat` (required) and `nbf` (60 s leeway), a lifetime (`exp` − `iat`) of at most one hour, **prefix confinement** (every path `path_auth` grants, `root`+`pub` and `root`+`sub`, lies within the signing key's prefix at a `/` boundary; `.` and `..` segments are refused), and that `path_auth` covers the announced path. Other claims are ignored. On a managed relay every key carries its project's prefix, so a key can never sign for another tenant; the trust snapshot's keys without a prefix are ignored. Statically configured keys have no prefix.

**Publisher contract and credential lifetime.** A publisher serves a track named `auth` on each broadcast path it announces. Each group on that track carries one credential as raw JWT bytes, at most 8 KiB.
- The first group must arrive within 5 s of the announcement.
- **Hard expiry:** the relay ends the session at the current credential's `exp` + 60 s, with MoQ error code `0x2` (Unauthorized) and the reason `credential_expired`.
- **In-session refresh:** before `exp`, the publisher writes a fresh credential as a new group on the same `auth` track (recommended at 80% of the lifetime). The relay accepts it only if all of these hold:
  - it passes every check above;
  - its key is active and belongs to the same project (the `kid` may change, so key rotation needs no reconnect);
  - it grants nothing the current credential doesn't;
  - it still covers the announced path.
- An accepted refresh replaces the session's credential and deadline. A refused one leaves both unchanged.
- At most one refresh per 10 s is verified per `auth` track.
- Metric: `qumo_relay_credential_refreshes_total{result}`, where `result` is `accepted`, `checks`, `other_project`, `widens`, `uncovered_path`, `rate`, `size` or `stale`.

**Subscriber contract.** With credential auth on, there's no anonymous subscribe. A client that subscribes (over WebTransport, or over native QUIC from outside the trusted peers) announces the reserved broadcast `/.qumo/session` with an `auth` track carrying its **session credential**.
- A SUBSCRIBE is admitted only if that credential's `path_auth` subscribe grant (`root`+`sub`) covers the requested path. A SUBSCRIBE sent before the credential is accepted waits up to 5 s for it. A refused SUBSCRIBE is closed with subscribe error `0x4` (Unauthorized).
- The session credential follows the publisher lifecycle: hard expiry and in-session refresh. A refresh must still cover every open subscription.
- The `/.qumo/session` announcement must stay up for the session's life. Withdrawing it while connected ends the session, with reason `credential_retracted`. A second session credential announcement is ignored.
- Broadcasts under `/.qumo/` are relay control and are never routed.
- Trusted peers (`PEER_CIDRS` / mTLS) are not gated. Clients such as the HLS egress, `qumo loadgen` and `qumo smoketest` must run as trusted peers against a relay with credential auth on.
- Metric: `qumo_relay_subscribe_authorizations_total{result}`, where `result` is `admitted`, `not_covered`, `no_credential` or `unidentified`.

`QUMO_RELAY_AUDIENCE` and `QUMO_CREDENTIAL_ISSUER` are no longer read. Introspection (`POST /v1/credentials/introspect`) and fetching the control plane's key set are removed.
