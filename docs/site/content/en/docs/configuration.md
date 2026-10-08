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
| `PEERS` | (empty) | Comma-separated relays to dial, as `host:4433`. Each host is resolved to all its addresses and every address is dialed, so a DNS name for a group of relays (`role-hub.qumo-relay.service.consul:4433`) connects to each; a relay that reaches itself that way drops that session. The node relays their announcements. Needs `CA_FILE` and a peer identity (below). |

There is no runtime peer-discovery service — the list is static, resolved and
dialed once at startup and re-dialed with backoff on disconnect. A relay
that starts later must dial the earlier ones; they learn of it only when
they restart. Every resolved address is dialed and retried for as long as
the relay runs, so name peers by addresses they listen on: `localhost`
resolves to `::1` as well, and a relay bound to `127.0.0.1` leaves that
dial warning forever. A peer that stops cleanly (SIGINT, GOAWAY) is
re-dialed at once; one that vanishes without closing is noticed after the
QUIC idle timeout, 60 s. See
[Deployment → Peer topology]({{< relref "deployment/peer-topology" >}}) for
how they fit together, and [Deployment → Nomad]({{< relref "deployment/nomad" >}})
for a worked example of giving relays stable addresses on Nomad.

## Peer trust (optional)

Relays authenticate each other with mutual TLS under a CA of yours, the
**relay CA**. A relay's peer identity is a certificate that CA issued, separate
from the public certificate it serves browsers: different credentials, different
trust. `PEERS` only says whom to dial; identity comes from the certificates.

| Variable | Default | Description |
|---|---|---|
| `CA_FILE` | (empty) | PEM certificate of the relay CA. A native-QUIC session whose client certificate it verifies is an **internal client** (it may subscribe to anything and announce nothing), or a **relay peer** (served without a credential, checked for nothing) when the certificate also carries the peering name. Unset: no session is either. |
| `PEER_CERT_FILE` / `PEER_KEY_FILE` | (empty) | This relay's peer identity: a certificate `CA_FILE` issued for the peering name, usable for client and server authentication. Set together, and only with `CA_FILE`. It is presented to the relays this one dials and to the relays that dial it. |

The four valid settings:

- nothing: a standalone relay;
- `CA_FILE` alone: internal clients such as the HLS egress are authenticated, but this relay is no peer;
- `CA_FILE` and a peer identity: this relay accepts peers but dials none;
- all three with `PEERS`: full peering.

The relay refuses to start with only one of the two peer files, a peer identity without `CA_FILE`, `PEERS` without all three, or a peer certificate the CA didn't issue, that lacks the peering name, or that isn't valid. A browser is never asked for a certificate.

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

A client puts its credential in the connect URL: `https://relay.example.com/…?jwt=…` over WebTransport, or `moqt://relay.example.com/…?jwt=…` over native QUIC. **The relay verifies the credential itself, against a key set** (`QUMO_AUTH_KEYS`); no other process is involved.

Without a key set, auth is off: the relay admits every session unchecked and logs a warning at startup, the right setting for local development and for a relay that is open on purpose.

| Variable | Default | Description |
|---|---|---|
| `QUMO_AUTH_KEYS` | (unset) | The key set: a JWK Set of Ed25519 public keys, what [`qumo auth keygen`](../cli/auth/#keygen) writes. Its form says where it is: an `https://` URL (or `http://` on a loopback host only) is downloaded about every 30 s (±10%, so relays restarted together spread out) with `If-None-Match`; a path or a `file://` URL is a file, re-read when it changes. Any other scheme is refused at startup. |
| `QUMO_AUTH_KEYS_CACHE` | (unset: no cache) | A file the relay keeps the last downloaded key set in, and loads at startup, so a relay that restarts while the key-set URL can't be reached still has its keys. For a key-set URL only. |
| `QUMO_RELAY_TOKEN` | (unset) | Sent as a bearer token to a key-set URL and to `QUMO_USAGE_URL`. |
| `QUMO_USAGE_URL` | (unset: no reports) | Where the relay reports each verified session's usage (below). Needs a key set. |

**Relay peers are never checked:** sessions whose client certificate `CA_FILE` verifies and that carries the peering name, and peers this relay dials (`PEERS`). A session whose certificate `CA_FILE` verifies without the name is an **internal client**: it needs no credential either, and may subscribe to anything and announce nothing ([Peer trust](#peer-trust-optional)).

### Verifying against a key set
A credential is a token your app signs with its own key, using the Go package [`github.com/qumo-dev/qumo/token`](../cli/auth/) (or `qumo auth token` while testing). The relay admits a session when, in order: the token's header carries no `crit`, its `alg` is EdDSA and its `kid` is in the key set; the signature verifies; its claims are exactly `path_auth`, `iat`, `nbf`, `exp` and an optional `jti`; the times hold, with 60 s leeway and a lifetime of at most an hour; and **every path it grants lies within its key's `prefix`**. The session may then publish and subscribe where the token says, and ends when the token expires.

Each key in the set may carry two members besides the standard JWK ones:

```json
{"keys": [{"kty": "OKP", "crv": "Ed25519", "x": "…", "kid": "…", "prefix": "acme/app", "pause": ["publish"]}]}
```

- **`prefix`** confines the key: a token signed with it may grant only paths at or beneath it.
- **`"pause": ["publish"]`** starts no new sessions that may publish, for example while the key's owner is at a limit on broadcasts. Sessions that only subscribe still start, and live sessions continue: a pause stops what is new, not what is on air. To cut a key's live sessions off, remove the key from the set. `"publish"` is the only thing a key can pause; any other entry makes the set invalid, so a misspelling can't leave a key unpaused. The older `"publish": false` still means the same and is deprecated.

**Live sessions are re-checked every 30 s** against the current set: a session whose key has left the set ends, with MoQ `0x2` (Unauthorized), so removing a key cuts its sessions off within about a minute at worst (one refresh plus one re-check), about 30 s on average. If the set can't be refreshed, the relay keeps the last one and logs an error; after 6 hours without a refresh it admits no new sessions, while live ones run to their expiry. Before the first successful load it admits nothing. With `QUMO_AUTH_KEYS_CACHE` set, the last downloaded set is loaded at startup, and its age counts toward the 6 hours, so a restart doesn't extend how long an old set is trusted. A key set *file* that can't be read stops the relay at startup, rather than leaving it running and refusing everyone.

### Usage reports
With `QUMO_USAGE_URL` set, the relay POSTs a JSON array of records to it every 10 s:

```json
[{"type": "session_open", "session_id": "…", "kid": "…", "role": "publish", "ts": "…"},
 {"type": "usage", "session_id": "…", "kid": "…", "role": "publish",
  "metrics": {"gateway.ingress_bytes": 10162, "gateway.egress_bytes": 9223}, "ts": "…"},
 {"type": "session_close", "session_id": "…", "kid": "…", "role": "publish",
  "metrics": {"gateway.ingress_bytes": 10438, "gateway.egress_bytes": 9165}, "reason": "closed", "ts": "…"}]
```

`role` is `publish`, `subscribe` or `both`, from what the token grants. Byte counts are cumulative and from the relay's point of view (what it received is ingress), so a receiver can take a resent record without counting it twice.

**Sessions that may publish are reported one by one,** as above: an open, usage every 30 s, and a close.

**Sessions that only subscribe are reported together.** Viewers outnumber publishers by orders of magnitude, and a record per viewer would make the receiver's load grow with the audience. Their bytes are added up per key into one running total since the relay started, sent as a `usage` record whose `session_id` is `viewers.<run>.<kid>`, where `<run>` is new each time the relay starts (so a restart begins new totals rather than lowering old ones). There is no open or close for them, and a total is sent only when it has changed.

Records go out in batches of at most 500. A failed send, a 401, 403, 408, 413 or 429 included, keeps the records and retries them (logged once, then again when it recovers); only a batch the receiver can't read (400 or 422) is dropped. When the relay shuts down, it waits up to 5 s for its last sessions to record their end before the final send.

### What a session may do
- **Refusal:** a WebTransport client gets the HTTP status before the upgrade: 401 for a credential that can't be accepted, 403 for a valid one that may not do what it asks, or 503 while the relay has no usable key set. A native-QUIC session is closed with `0x2` (Unauthorized).
- **Announcements** are routed only if the token's publish path covers their path.
- **Subscriptions** are served only if the token's subscribe path covers their path. A refused subscription gets the same answer as a path that doesn't exist (`NotFound`).
- **Expiry:** a session ends when its token expires (its `exp` plus the leeway), closed with `0x2` (Unauthorized) and reason `expired`. This covers publishers and subscribers. The client reconnects with a fresh credential, ideally shortly before the credential's `exp`. The deadline is taken on the relay's monotonic clock when the session is admitted, so a wall-clock jump doesn't move it.
- **Why a session ended** is the `reason` of its usage record: `expired`, `refused` (its key left the set), `closed` (the client or relay closed it normally), `dropped` (the connection was lost) or `upgrade_failed` (a WebTransport session that was admitted, whose upgrade then failed, so it never started).
- **Relay peers**, peers this relay dials, and internal clients carry no credential, so nothing above applies to them.
- **Not yet enforced:** which paths a session can discover (qumo-dev/qumo#450). Announce interest lists every path under the requested prefix, and a TRACK request returns a track's publisher properties (TRACK_INFO) for any path. Both reveal path names and metadata, never media.

Metrics: `qumo_relay_auth_requests_total{event,result}` (`admitted`, `refused`, `error`, and `unchecked` with auth off), `qumo_relay_announcements_refused_total`, `qumo_relay_subscribe_authorizations_total{result}` (`admitted`, `not_covered`) and `qumo_relay_sessions_ended_total{reason}` (`expired`, `refused`). `auth_requests_total`'s `event` is `connect`, `revalidate` or `end`; an `end`'s `result` is `ok` or `error`.
