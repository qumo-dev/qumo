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

## Session auth (optional)

A client puts its credential in the connect URL: `https://relay.example.com/…?jwt=…` over WebTransport, or `moqt://relay.example.com/…?jwt=…` over native QUIC. The relay checks it in one of two ways:

- **It verifies the credential itself, against a key set** (`QUMO_AUTH_KEYS`). This is the usual setup: no other process is involved.
- **It asks an auth server** (`QUMO_AUTH_URL`), for policy beyond keys and paths.

With neither, auth is off: the relay admits every session unchecked and logs a warning at startup, the right setting for local development and for a relay that is open on purpose. Setting both is an error.

| Variable | Default | Description |
|---|---|---|
| `QUMO_AUTH_KEYS` | (unset) | The key set: a JWK Set of Ed25519 public keys, what [`qumo auth keygen`](../cli/auth/#keygen) writes. Its form says where it is: an `https://` URL (or `http://` on a loopback host only) is downloaded about every 30 s (±10%, so relays restarted together spread out) with `If-None-Match`; a path or a `file://` URL is a file, re-read when it changes. Any other scheme is refused at startup. |
| `QUMO_RELAY_TOKEN` | (unset) | Sent as a bearer token to a key-set URL and to `QUMO_USAGE_URL`. |
| `QUMO_USAGE_URL` | (unset: no reports) | Where the relay reports each verified session's usage (below). Needs a key set. |
| `QUMO_AUTH_URL` | (unset) | An auth server the relay asks when each client session connects. `https://`, or `http://` on a loopback host only. |

**Trusted peers are never checked:** sessions with a client certificate verified against `CA_FILE`, and peers this relay dials (`PEERS`).

### Verifying against a key set
A credential is a token your app signs with its own key, using the Go package [`github.com/qumo-dev/qumo/token`](../cli/auth/) (or `qumo auth token` while testing). The relay admits a session when, in order: the token's `kid` is in the key set and its `alg` is EdDSA; the signature verifies; its claims are exactly `path_auth`, `iat`, `nbf`, `exp` and an optional `jti`; the times hold, with 60 s leeway and a lifetime of at most an hour; and **every path it grants lies within its key's `prefix`**. The session may then publish and subscribe where the token says, and ends when the token expires.

Each key in the set may carry two members besides the standard JWK ones:

```json
{"keys": [{"kty": "OKP", "crv": "Ed25519", "x": "…", "kid": "…", "prefix": "acme/app", "admit": false}]}
```

- **`prefix`** confines the key: a token signed with it may grant only paths at or beneath it.
- **`"admit": false`** keeps the key's live sessions and starts no new ones, for example while rotating away from it.

**Live sessions are re-checked every 30 s** against the current set: a session whose key has left the set ends, with MoQ `0x2` (Unauthorized), so removing a key cuts its sessions off within about a minute at worst (one refresh plus one re-check), about 30 s on average. If the set can't be refreshed, the relay keeps the last one and logs an error; after 6 hours without a refresh it admits no new sessions, while live ones run to their expiry. Before the first successful load it admits nothing.

### Usage reports
With `QUMO_USAGE_URL` set, the relay POSTs a JSON array of records to it every 10 s:

```json
[{"type": "session_open", "session_id": "…", "kid": "…", "role": "publish", "jti": "…", "ts": "…"},
 {"type": "usage", "session_id": "…", "kid": "…", "role": "publish",
  "metrics": {"gateway.ingress_bytes": 10162, "gateway.egress_bytes": 9223}, "ts": "…"},
 {"type": "session_close", "session_id": "…", "kid": "…", "role": "publish",
  "metrics": {"gateway.ingress_bytes": 10438, "gateway.egress_bytes": 9165}, "reason": "closed", "ts": "…"}]
```

`role` is `publish`, `subscribe` or `both`, from what the token grants; `jti` is the token's, when it has one. Byte counts are cumulative and from the relay's point of view (what it received is ingress), so a receiver can take a resent record without counting it twice. Usage is reported every 30 s per live session; records go out in batches of at most 500; a failed send, a 401, 403, 408, 413 or 429 included, keeps the records and retries them, and only a batch the receiver can't read (400 or 422) is dropped. When the relay shuts down, it waits up to 5 s for its last sessions to record their end before the final send.

### Asking an auth server
The auth server holds the policy: which credentials it accepts, and what a session that presents none may do. [`qumo auth`](../cli/auth/) is one; any server that speaks the contract below works too.

#### The auth server contract
A subset of [`moq-auth`](https://github.com/kixelated/moq/blob/main/doc/bin/relay/auth.md). When a client connects, the relay POSTs JSON:

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
- **Subscriptions** are served only if the grant's `subscribe` patterns cover their path. A refused subscription gets the same answer as a path that doesn't exist (`NotFound`).
- **Expiry:** a session ends at its grant's `expires` (Unix seconds), closed with `0x2` (Unauthorized) and reason `expired`. This covers publishers and subscribers. The client reconnects with a fresh credential, ideally shortly before the credential's `exp`. A grant without `expires` never expires. The deadline is taken on the relay's monotonic clock when the grant is accepted, so a wall-clock jump doesn't move it; any leeway is the auth server's to add.
- **Revalidation:** every `revalidate` seconds, the relay sends the session's connect request again, with the same `id` and `event: "revalidate"`. This is how key revocation, project suspension or a spend limit reaches a live session.
  - **A 401 or 403** ends the session with `0x2` (Unauthorized) and reason `refused`.
  - **A grant that can't be enforced** ends it with reason `invalid`.
  - **An admitted grant** keeps the session and takes the new `expires` and `revalidate`. Its patterns are not compared: they were fixed at connect, and the credential is the same. A grant without `expires` keeps the session's current deadline; a revalidate never lifts it.
  - **Anything else** (a timeout, a 5xx) is retried with jittered exponential backoff, from about 1 s up to 30 s. The session lives until its current `expires`, so an outage is bounded by what the server last granted.
- **Usage and session end:** each revalidate carries the session's cumulative `bytes` (`{"sent": …, "received": …}`, counted from the relay's side). `bytes` is left out while both totals are zero, so an absent one means none. When a checked session closes, the relay sends `event: "end"` with the same `id`, the final `bytes`, `duration` in whole seconds, and a `reason`: `expired`, `refused`, `invalid`, `closed` (the client or relay closed it normally), `dropped` (the connection was lost) or `upgrade_failed` (a WebTransport session the auth server admitted, whose upgrade then failed, so it never started). A request whose `Origin` the relay refuses is never sent to the auth server. The end report is best effort: one attempt with a 2 s timeout, sent after the close, so a slow auth server can't hold a session open. A lost one costs at most one revalidate interval of usage. Reporting bytes on revalidate as well as end is qumo's one extension of moq-auth, so billing sees a long session before it ends.
- **Trusted peers**, and peers this relay dials, are never checked.
- **Not yet enforced:** which paths a session can discover (qumo-dev/qumo#450). Announce interest lists every path under the requested prefix, and a TRACK request returns a track's publisher properties (TRACK_INFO) for any path. Both reveal path names and metadata, never media.

Metrics: `qumo_relay_auth_requests_total{event,result}` (`admitted`, `refused`, `invalid`, `error`, and `unchecked` with auth off), `qumo_relay_announcements_refused_total`, `qumo_relay_subscribe_authorizations_total{result}` (`admitted`, `not_covered`) and `qumo_relay_sessions_ended_total{reason}` (`expired`, `refused`, `invalid`). `auth_requests_total`'s `event` is `connect`, `revalidate` or `end`; an `end` report's `result` is `ok` or `error`.
