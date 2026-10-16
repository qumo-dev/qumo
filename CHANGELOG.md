# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

> **Breaking for key-set servers.** `"publish": false` no longer pauses a key; write `"pause": ["publish"]` instead.

> **Breaking for usage receivers.** Usage records drop `role`, and viewer totals arrive as `viewers` records with a `run`; update receivers before relays.

> **Breaking for dashboards.** Session metrics gain a `class` label and `transport` no longer says `peer` or `internal`; update queries.

> **Breaking for web clients.** Relays on gomoqt v0.25.0 send no ENDED when closing an announce stream; move browsers to `@qumo/moq` 0.25.0 first.

> **Breaking for the HLS egress.** A relay-CA certificate without the peering name no longer admits a session; give the egress `RELAY_SIGNING_KEY` (new) or a peer certificate.

> **Breaking for apps, funnels and long-lived clients.** A token's `root` and a key's `prefix` are MoQ directories (`/acme/room/42/`), and every grant, a session's own included, lapses at its token's `exp`; a client that stays longer presents a fresh token on the session's Auth Stream.

> **Breaking for apps and funnels.** Tokens now carry a `root` plus path patterns per action (`publish`, `subscribe`, `create`, `post`, `redact`) and a token with `scopes` is refused; move relays, funnels and token signers together. `qumo_relay_announcements_refused_total` and `qumo_relay_subscribe_authorizations_total` are gone.

### Added

- **Tickets: a session's grants can grow (`internal/relay`, `internal/auth`).** A client presents further tokens on MoQ Auth Streams (any `token.Sign` token, signed by the key of the session's credential, each lasting until its `exp`, at most 33 credentials per session including its own), so an app keeps one session per client and grants rooms as the client joins; counted by `qumo_relay_tickets_total{result}`. A ticket that can't be checked (key set unusable) is answered `INTERNAL` (retryable), a refused one `UNAUTHORIZED`, and with auth off or for a relay peer `NOT_SUPPORTED`. A paused key still renews a grant that may publish the session already holds.
- **A token may grant nothing (`token`).** A token with no root and no patterns admits a session that can do nothing until a ticket grants it.
- **Long-lived clients renew their relay credential (`internal/relaycred`, `internal/funnel`, `internal/hls`).** With `RELAY_SIGNING_KEY`, the funnel and the HLS egress present a fresh credential on the Auth Stream two thirds through the grant, and redial if none is granted in time; a credential fixed in `RELAY_URL` can't be renewed.
- **The HLS egress signs its own relay credential (`internal/hls`).** With `RELAY_SIGNING_KEY`, `qumo hls` signs a fresh `subscribe`-only credential for `RELAY_TRACK_PATH` on every connect, fixing egresses that stopped reconnecting after their fixed `?jwt=` token expired; contradictory or unusable credential settings now stop it at startup.
- **Richer session logs (`internal/relay`).** `relay: new session` now logs `id`, `class`, `transport`, `path`, `sub`, `moq`, `alpn`, `tls`, `sni` (plus `offered`, `user_agent`, `origin` for HTTP upgrades); a new `relay: session ended` line gives `reason`, `duration` and bytes.
- **Configurable QUIC transport for the relay and every QUIC command (`internal/quicconfig`).** Nine `QUIC_*` variables set initial packet size, path MTU discovery, keep-alive, idle/handshake timeouts and receive windows (the timeouts, keep-alive and initial windows also apply to WebSocket); invalid values stop startup. Set `QUIC_INITIAL_PACKET_SIZE=1200` on 1280-MTU paths such as Tailscale, where no session could open before.
- **The usage report format is a Go package (`usage`).** Receivers of `QUMO_USAGE_URL` can decode and check `usage.Record` with `Record.Validate` instead of keeping their own copy.
- **`token.Key` carries its pause (`token`).** `Key.Pause` is written and read as `"pause"` by `MarshalKeySet`/`ParseKeySet`, so key-set servers no longer hand-write the JSON.

### Changed

- **Tokens carry a root and path patterns per action, matching gomoqt's `moqt.Grant` (`token`, `qumo auth token`, `internal/auth`, `internal/funnel`, `internal/hls`).** `root` is a directory (`/acme/room/42/`, or `/`) at or beneath the signing key's prefix, now also a directory (bare prefixes still load); every grant, a session's own credential included, lasts until its token's `exp`, after which the session may do nothing until a fresh token is presented, patterns are relative to it (`/7`, `/7/`, `/`, `{name}` wildcards), `subscribe` also covers history (no `fetch`), and the root's own path is not a broadcast.
  - To move over: `qumo auth token` takes `-root DIR` and repeatable `-publish`, `-subscribe`, `-create`, `-post`, `-redact` in place of `-scope`; `qumo auth keygen -prefix` defaults to `/`; in Go, `token.Grant` embeds `moqt.Grant` and replaces `token.Scope`, and `auth.NewGrant` takes a `token.Grant`.
  - `path_auth` is read as a grant (`root` as the directory it names, `pub`→`publish`, `sub`→`subscribe`); the funnel's own relay credential has `RELAY_PUBLISH` as its root and publishes every path beneath it, no longer the path itself.
- **gomoqt now enforces each relay session's grants (`internal/relay`).** Subscriptions, TRACK and FETCH outside the grants are refused with `UNAUTHORIZED` (not `NotFound`), announcements are listed only within a grant's root, and what an ending ticket covered is cancelled. `qumo_relay_announcements_refused_total` and `qumo_relay_subscribe_authorizations_total` are removed.
- **gomoqt v0.25.0 (`go.mod`, `playground`).** Closing an announce stream sends no ENDED (web clients need `@qumo/moq` 0.25.0), `https://` sessions a command dials now honour `QUIC_*`, with `QUIC_KEEPALIVE=0` WebSocket sessions no longer ping, and a fetch the relay can't serve is reset with `InternalGroupErrorCode`, not `FetchErrorCodeInternal`.
- **Session metrics split transport from class (`internal/relay`).** `qumo_relay_sessions_total`, `qumo_relay_sessions_closed_total` and `qumo_relay_session_bytes_total` gain `class` (`client`/`peer`) and `transport` is only `webtransport`, `websocket` or `quic`; use `class="peer"` for `transport="peer"` and add `class="client"` to old `transport="quic"` queries.
- **`"publish": false` is no longer read (`token`, `internal/auth`).** Key-set servers must write `"pause": ["publish"]` before relays upgrade, or paused keys stop being paused.
- **Usage records have no `role` (`internal/auth`, `usage`).** Sessions with a `publish` pattern are reported on their own; every other session is a viewer counted in a `viewers` record with `run` and `kid`, which also stops empty-role records making receivers drop whole batches.
- **The Docker image builds without QEMU emulation (`docker/Dockerfile`).** It embeds the committed `playground/dist` and cross-compiles Go, fixing the crashing arm64 build; pull requests now build arm64 too.

### Removed

- **Scopes and track matches (`token`, `qumo auth token`, `internal/funnel`).** `scopes`, `token.Scope` and `-scope` are gone; a grant reaches every track of the broadcasts it matches, and funnel tracks are created only by a `PUT` with a `create` pattern (`token.ActionCreate`).
  - Records into a missing track get `404` (qumo-ledger v0.4.0), so apps create tracks before senders post; a funnel without `LEDGER_URI` forgets them on restart.
  - Only `publish` patterns count for `"pause": ["publish"]` and usage; a paused key's funnel `PUT` with `create` is refused.
- **The internal client (`internal/relay`).** A relay-CA (`CA_FILE`) certificate without the peering name no longer grants subscribe-anything access; the HLS egress uses `RELAY_SIGNING_KEY` or a peer certificate in `RELAY_CERT_FILE`, and refuses to start with its old certificate.

### Fixed

- **A viewer that subscribes before a publisher’s tracks are known now gets them (gomoqt v0.26.0, `@qumo/moq` 0.26.0).** An ingest’s catalog subscription stays open and receives each change, so a player no longer stays on an empty catalog; the playground applies the newest catalog when updates arrive out of order.
- **The relay closes an upstream subscription when it stops forwarding a track,** so publishers no longer keep sending groups nobody accepts.
- **Draining a route and shutting down end a subscribe still waiting for its first group (gomoqt v0.24.2).** Quiet tracks now fail over on drain, and `qumo hls` waiting for a catalog exits on SIGTERM.
- **Subscribers of a quiet track are no longer dropped after 30 seconds (gomoqt v0.24.1).** Tracks with no group yet (an idle event track, a muted speaker) no longer fail with `subscription timed out`.
- **The relay delivers tracks whose group sequences don't start at 1 or have gaps (`internal/relay`).** Such tracks, including funnel tracks with more than one record, were never delivered before.

## [v0.14.261010] - 2026-10-10

> **For operators.** `RELAY_ADDR`'s TCP port now also takes WebSocket over TLS: open it for TCP wherever it is open for UDP, or set `WS_ENABLE=0`.

> **Breaking for funnel senders.** A funnel takes posts only with a `post` scope (a `publish` scope or a `path_auth` token's `pub` no longer posts); sign senders' credentials with `post`.

### Added

- **The playground uses WebSocket on WebKit (Safari, every iOS browser) and WebTransport elsewhere (`playground/src/transport.ts`, #59).** `?transport=websocket` or `?transport=webtransport` overrides the choice.
- **WebSocket fallback for clients whose WebTransport does not work (`qumo relay`, #59).** On by default, served with TLS (`CERT_FILE`/`KEY_FILE`) on `RELAY_ADDR`'s own TCP port next to plain HTTP, so `https://relay:4433` and `wss://relay:4433` both work; sessions are admitted exactly like WebTransport ones and share tracks with them.
  - `WS_PORT` moves the TLS side to its own port, `WS_PORT=0` leaves TLS to a proxy, `WS_ENABLE=0` turns WebSocket off; an invalid or unavailable `WS_PORT` stops the relay at startup.
  - The subprotocol is `qmux-02.moq-lite-05`; `CORS_ALLOWED_ORIGINS` is the only origin check; a refused client is told in-session with MoQ `Unauthorized` (`refused`/`unavailable`).
- **Per-transport session metrics (`qumo relay`).** `qumo_relay_sessions_total{transport}`, `qumo_relay_sessions_closed_total{transport,reason}` and `qumo_relay_session_bytes_total{transport,direction}`.
- **A `post` action (`token`, `qumo auth token -scope`).** It grants sending messages into a funnel's track and never publishing through a relay; in Go, `token.ActionPost`.
- **A `redact` action (`token`, `qumo auth token -scope`, `internal/funnel`).** It grants removing a record from a funnel's track (`DELETE ?group=`), with the credential's `sub` as sender; in Go, `token.ActionRedact`. Older verifiers refuse any token listing `redact`, so keep such credentials separate until every relay and funnel is upgraded.

### Changed

- **Bumped `github.com/qumo-dev/gomoqt` to v0.23.0.** Safari clients can now publish over WebTransport (the relay grants initial stream limits), and a client offering only unsupported protocols is refused with `400`.
- **A funnel takes posts with `post`, not `publish` (`internal/funnel`).** `POST`/`PUT` need a matching `post` scope; `publish` only sends through a relay, and a `path_auth` `pub` no longer posts (its `sub` still reads history).
- **`"pause": ["publish"]` also covers a `post` scope on every track of a broadcast (`internal/auth`).** A `post` scope naming one track is treated as a viewer and is not paused.
- **qumo-ledger v0.3.0 (`internal/funnel`).** Adds redaction: `DELETE /tracks/…?group=<group>` publishes `{"redacts": "<group>"}`, deletes the payload and marks the record `"redacted": true` in history.

## [v0.13.261009] - 2026-10-09

> **Breaking for Go callers and funnel senders.** `token.Grant` is now `{Scopes, Subject}` and `token.Sign` writes `scopes`, never `path_auth`; a funnel's sender is now the credential's `sub`, not a path segment.

> **Behavior change for apps.** A session no longer ends when its credential expires; end it by withholding the next credential or removing the key from the set.

### Added

- **Credentials grant actions on exact tracks and name their bearer (`token`, `internal/auth`).** A `scopes` claim lists `actions` (`publish`, `subscribe`, `fetch`), a `broadcast` match (`exact` or `prefix`) and an optional `track` match, plus `sub`; on the CLI, `qumo auth token -scope ACTIONS:BROADCAST[:TRACK]` (repeatable, `a/b/**` for a prefix) and `-sub`.

### Changed

- **Breaking: one grant model, scopes (`token`, `internal/auth`, `internal/funnel`, `internal/relay`).** `path_auth` from other signers is still accepted and read as scopes, and `-publish`/`-subscribe` remain as shorthands.
  - Upgrade relays and funnels before the apps that sign tokens: an older relay refuses the `scopes` claim.
  - A funnel records with a matching `publish` scope as the credential's `sub` and reads history with `fetch`; only a `publish` scope covering every track announces a broadcast or counts as a publisher.
- **Behavior change: a credential's expiry decides whether a session may start, not how long it lives (`token`, `internal/auth`).** The relay no longer ends sessions with reason `expired` (`qumo_relay_sessions_ended_total{reason}` counts only `refused`), key-set removal still ends sessions within 30 s, and a `reval` claim is refused; Go adds `token.VerifyLive`.
- **Pause a key's new publishing sessions with `"pause": ["publish"]` (`internal/auth/keyset.go`).** Any other `pause` entry, duplicate member, or a set without a `"keys"` list now makes the set invalid, so the relay keeps its last good set (`{"keys": []}` is still a valid empty set).

### Deprecated

- **`"publish": false` on a key.** Write `"pause": ["publish"]` instead; the relay logs a warning naming keys that use it, and it will be removed later.

### Fixed

- **qumo-ledger v0.2.2 (`qumo funnel`, `qumo hls`).** An unreadable store is reported as an error when seeking by time, wide epochs/sequences no longer lose bits in group IDs, and fragments with a short `trun` are refused.

### Security

- **Built with Go 1.27.2 and `golang.org/x/net` v0.60.0, fixing GO-2026-6603 to GO-2026-6617.** These affect `net/http`, HTTP/2, `crypto/tls`, `net/textproto` and `os`, which the relay's WebTransport and HTTP paths use; `go.mod` now requires `1.27.2`.
- **`CA_FILE` is read through `os.Root`,** so a symlink can't lead it outside the working directory.

## [v0.12.261008] - 2026-10-08

> **Breaking for operators.** Relay peers now use mutual TLS under a relay CA: `PEERS` needs `PEER_CERT_FILE`, `PEER_KEY_FILE` and `CA_FILE`, so issue a peer certificate per relay and set them.

### Added

- **`qumo funnel`: many HTTP senders publish JSON records into one MoQT track (`internal/funnel`).** Each record is stored in qumo-ledger (`LEDGER_URI`: file, Postgres/CockroachDB, S3 or memory) before it is sent, optionally through a relay (`RELAY_URL`, with `RELAY_SIGNING_KEY`/`RELAY_PUBLISH`, `RELAY_CA_FILE`, `RELAY_TLS_INSECURE`). Configured by `FUNNEL_ADDR` (default `:8090`), `FUNNEL_SERVE_ADDR`, `FUNNEL_SENDER_LIMIT` and `FUNNEL_TRACK_LIMIT`; its HTTP listener is plain HTTP, so terminate TLS in front of it.
- **The playground's DevTools timeline can be zoomed, dragged back in time, and returned to live (`playground/src/devtools/view.ts`).**
- **The playground can publish a test pattern with no camera (`playground/src/publish/pattern.ts`).** It shows a clock and beeps with a flash each second, for checking A/V sync and delay.

### Changed

- **Breaking: relay peers authenticate with CA-issued certificates in both directions (`internal/relay/peer_trust.go`).** A relay no longer presents `CERT_FILE` to peers; its peer identity is `PEER_CERT_FILE`/`PEER_KEY_FILE`, issued by `CA_FILE` with the DNS name `peer.qumo.internal`.
  - `PEER_CERT_FILE` and `PEER_KEY_FILE` go together and need `CA_FILE`; `PEERS` needs all three; a bad, expired or name-less peer certificate stops the relay at startup.
  - To move over, follow `docs/site/content/en/docs/deployment/tls.md`; for development, `PEER_NAMES=a,b mage cert` writes a relay CA and peer certificates.
- **The playground's publish board encodes by itself and no longer depends on `@okdaichi/av-nodes` (`playground/src/publish`).** Stopping now also releases the microphone, and ending a share from the browser's controls stops the run.

### Fixed

- **An RTSP pull no longer drops a healthy stream on the server's keepalive answer (`internal/rtsp`, `internal/ingest`).** Multi-word reasons, bodies, cameras without `GET_PARAMETER` and cameras that authenticate every request no longer force reconnects; `454 Session Not Found` reconnects at once via `rtsp.ErrSessionNotFound`.
- **A credential whose header carries `crit` is refused** (`token.Verify`, and so the relay).
- **Every HLS egress answer carries `Vary: Origin` (`internal/hls/cors.go`),** so a shared cache or CDN no longer serves a CORS-less answer to an allowed origin.
- **The playground's DevTools count stalls in background tabs too (`playground/src/devtools/stall_monitor.ts`).**
- **The playground dials the relay on the port it was started on and fetches HLS from the page's own host at port 8081.** `qumo playground --relay-addr` with a non-4433 port now works for the Webcam and HLS scenarios; `VITE_HLS_URL` still overrides under `mage web`.
- **A refused native-QUIC session is closed before its subscribes are answered,** so the client sees `unauthorized` rather than `track does not exist`.
- **The ingest no longer busy-loops while a new group is being stored (`internal/ingest`).**

## [v0.11.261005] - 2026-10-05

> **Breaking for operators.** The auth server (`QUMO_AUTH_URL`, `qumo auth` server mode) is removed and the relay verifies credentials itself; unset `QUMO_AUTH_URL`, set `QUMO_AUTH_KEYS` to the same key set, and stop the `qumo auth` process (see **Removed**).

### Added

- **The relay verifies credentials itself against a key set (`QUMO_AUTH_KEYS`).** Give it a file path, `file://` or `https://` URL (re-read, or polled every 30 s with `QUMO_RELAY_TOKEN` as a bearer token), from `qumo auth keygen`; a session whose key leaves the set ends within about a minute, and after 6 h without a successful refresh new sessions are refused.
- **Key-set and usage options for the relay.** `QUMO_AUTH_KEYS_CACHE` keeps the last downloaded set so a restart works while the URL is down, `"publish": false` on a key stops new publishing sessions without locking viewers out, and `QUMO_USAGE_URL` receives per-session usage as JSON, sent with the same bearer token (viewer-only sessions are summed per key, `session_id` `viewers.<run>.<kid>`).
- **An unreadable key-set file now stops the relay at startup** instead of refusing every session.
- **The playground's DevTools panel says in words how playback is going (`playground/src/devtools`).** A status line, a copyable log grouped into incidents with their likely cause, a clearer legend, and three timeline lanes.

### Changed

- **The playground's viewer decodes and plays audio and video itself and no longer uses `@okdaichi/av-nodes` (`playground/src/player`).** Its playback delay now adapts to measured arrival jitter (up to 500 ms), and a lost audio frame is one frame of silence instead of shrinking the buffer.
- **The playground's DevTools panel shows playback delay, arrival jitter and an audio-output lane, and is drawn on canvas (`playground/src/devtools`).** The panel no longer stalls the audio it shows.

### Removed

- **Breaking: the auth server is gone; the relay no longer reads `QUMO_AUTH_URL` and `qumo auth` no longer runs a server.**
  - To move over: unset `QUMO_AUTH_URL`, set `QUMO_AUTH_KEYS` to the key set the auth server read, stop `qumo auth`; tokens and signing keys are unchanged.
  - A relay with `QUMO_AUTH_URL` still set refuses to start. `qumo auth` needs `keygen` or `token`; `QUMO_AUTH_ADDR` and `QUMO_AUTH_KEYS_FILE` are no longer read.
  - The `invalid` result of `qumo_relay_auth_requests_total` and `invalid` reason of `qumo_relay_sessions_ended_total` are gone.

### Fixed

- **A playground opened at a host other than localhost dials that host (#456).** The relay address and the ffmpeg push commands shown for RTMP and RTSP now use the host from `/config`.
- **The playground's viewer no longer drops seconds of audio as it starts.**
- **Raising the playback delay interrupts the sound less often.**

## [v0.10.261005] - 2026-10-05

> **Breaking for operators.** `qumo_relay_sessions_expired_total` is replaced by `qumo_relay_sessions_ended_total{reason}`; use `reason="expired"` for the old count. Not yet authorized: which paths a session can discover through announce interest and TRACK_INFO (#450).

### Changed (breaking)

- **`qumo_relay_sessions_expired_total` is replaced by `qumo_relay_sessions_ended_total{reason}`** (`expired`, `refused`, `invalid`); use `reason="expired"` for the old count. `qumo_relay_auth_requests_total{event}` now also counts `revalidate`.

### Added

- **`qumo auth`: a ready-to-run auth server, key generator and token tool (#460).** `qumo auth keygen [-prefix acme/app]` writes an Ed25519 key and key set, `qumo auth` serves the relay (`QUMO_AUTH_KEYS_FILE`, `QUMO_AUTH_ADDR`, default `127.0.0.1:4440`), and `qumo auth token -publish … -subscribe … -ttl …` signs test tokens; apps sign with the Go package `github.com/qumo-dev/qumo/token`.
- **Live sessions are revalidated with the auth server (#419).** A 401 or 403 ends the session with `0x2` (Unauthorized), so key revocation and spend limits reach live sessions.
- **The auth server receives each session's byte counts and an `end` report (#424).** Bytes ride on every revalidate, and `end` carries the final bytes, duration and reason; counted in `qumo_relay_auth_requests_total{event="end"}`.
- **The HLS egress can connect as a trusted peer (#432).** Set `RELAY_CERT_FILE` and `RELAY_KEY_FILE` (from the CA the relay trusts as `CA_FILE`) with a `moqt://` `RELAY_URL`; its session is never auth-checked and never expires.
- **`qumo loadgen --relay` and `smoketest` `-pub`/`-sub` accept a `moqt://host:port/path?jwt=…` URL with a credential.**
- **The playground has a DevTools panel for subscribed tracks (`playground/src/devtools`).** It shows session bitrate and RTT, per-track rates and group outcomes, and a timeline of the last minute.

### Changed

- **Bumped `github.com/qumo-dev/gomoqt` to v0.22.1 (quic-go v0.63.0, webtransport-go `v0.13.0-okdaichi.2`).** WebTransport clients now see Unauthorized with reason `expired` or `refused` when the relay ends their session, so they can reconnect or stop (#423).
- **The playground's viewer no longer waits on a stalled group and plays audio and video on a shared delay (`playground/src/player`).** A group aborted part-way no longer ends the track, and a failed start no longer leaves read loops or subscriptions running.

### Fixed

- **RTMP and RTSP ingest no longer sends an old group in place of a new one,** which was heard as an occasional audio click.

### Security

- **A relay URL's credential is never logged (#432)** by `loadgen`, `smoketest` or the HLS egress.

## [v0.9.261004] - 2026-10-04

> **Breaking for operators.** Session auth now goes through an external auth server (`QUMO_AUTH_URL`; unset means auth off) and introspection is removed; `PEER_CIDRS`, `MTLS_REQUIRED` and `UPSTREAM_ADDR` are removed in favour of `CA_FILE` and `PEERS`. Sessions now end at their grant's `expires`; revalidation (#419) and announce/TRACK_INFO discovery (#418) are not yet enforced.

### Changed (breaking)

- **Relays ask an auth server when a session connects; introspection is removed (#417).** Set `QUMO_AUTH_URL`; without it auth is off (every session admitted, with a startup warning) and `QUMO_AUTH_PUBLIC` is gone (#441). Trusted peers are never asked, and peers this relay dials (`PEERS`) are now trusted too.
  - Removed: introspection (`POST /v1/credentials/introspect`), the `auth` track, `internal/credential`, `QUMO_CREDENTIAL_URL`, `QUMO_RELAY_TOKEN`, `QUMO_RELAY_AUDIENCE`, `QUMO_CREDENTIAL_ISSUER`, and usage reporting to `POST /v1/usage/events`.
  - New metrics: `qumo_relay_auth_requests_total{event,result}`, `qumo_relay_announcements_refused_total`. Requires gomoqt v0.21.0.
- **Subscriptions are checked against the session's grant (#418).** An uncovered SUBSCRIBE is refused with `NotFound`; new metric `qumo_relay_subscribe_authorizations_total{result}`. Path names and TRACK_INFO are still discoverable.
- **Peers are identified by a client certificate verified against `CA_FILE`; peer settings are reduced to `CA_FILE` and `PEERS`.**
  - Removed: `PEER_CIDRS`, `MTLS_REQUIRED`, `UPSTREAM_ADDR` (use `PEERS`, which now dials every address of each host).
  - Relays that relied on `PEER_CIDRS` need a private CA and a client certificate per relay before upgrading; `PEERS` are now verified against system roots plus `CA_FILE`.

### Added

- **Sessions end at their grant's `expires` (#423)** with `0x2` (Unauthorized), reason `expired`; new metric `qumo_relay_sessions_expired_total`.

### Changed

- **Bumped `github.com/qumo-dev/gomoqt` to v0.22.0.** Forwarded tracks end promptly, control messages are capped at 64 KiB, a client that fails SETUP is disconnected, and several leaks are fixed.

## [v0.8.260929] - 2026-09-29

### Added

- **Self-hosted relays can verify publisher credentials locally (`internal/credential`).** With `QUMO_RELAY_AUDIENCE` set (e.g. `qumo-relay-dev`), the relay checks credentials against the control plane's JWKS instead of calling `POST /v1/credentials/introspect`; there is no revocation feed (a revoked credential works until it expires) and no usage reporting, and `qumo-relay` is refused until #419.
- **`qumo loadgen subscribe --ramp R` paces session admission at R sessions/second (`internal/loadgen`).** The `tools/capacity` driver passes it through; the default (0) keeps the burst, and ramped runs record `ramp_per_sec` and `launched` in the JSONL row.

## [v0.8.260927] - 2026-09-27

### Changed

- **Native-QUIC sessions skip announcement auth only when identified as relay peers.** With `QUMO_CREDENTIAL_URL` set, a native-QUIC session is trusted only with an mTLS client certificate verified against `CA_FILE` or from a network in the new `PEER_CIDRS` setting (e.g. `100.64.0.0/10`); operators must set one of these for relay-to-relay and ingress links, and relays without credential auth are unaffected.

## [v0.7.260927] - 2026-09-27

### Added

- **Credential introspection now sends the announced `broadcast_path`.** `POST /v1/credentials/introspect` carries it alongside `token` so a credential server can scope credentials to a path; results are cached per (token, path), and servers that ignore the field are unaffected.
- **Installer scripts get an interactive console UI (`install.ps1`, `install.sh`).** A download spinner and check-mark milestones, with styling turned off when output is not a terminal.

## [v0.7.260922] - 2026-09-22

### Added

- **One-line installation scripts (`install.ps1`, `install.sh`).** They detect OS and architecture, verify SHA-256 checksums from GitHub Releases, install to `~/.qumo/bin` and set up `PATH`; also served from the docs site at `/install.ps1` and `/install.sh`.
- **Per-track relay subscription and upstream request metrics (`internal/relay`).** Prometheus gauges and counters for active subscriptions, distributor reuse, and upstream requests, errors and duration, labeled by broadcast path and track name.
- **`/routes` endpoint shows a snapshot of active routes (`internal/relay`).** Lists each broadcast path's upstream source, hop count, RTT, bitrate and last route update, for incident investigation.
- **`UPSTREAM_ADDR` connects edge and hierarchical relays to upstream relays (`internal/relay`).** A DNS hostname (e.g. `role-hub.qumo-relay.service.consul:4433`) connects to every resolved A/AAAA record, with automatic reconnect and backoff.

### Changed

- **Ingest fan-out to many subscribers is much cheaper per frame (`internal/ingest`).** Frame notification no longer walks one channel per subscriber, so cost stays flat as subscriber count grows.
- **Bumped `github.com/qumo-dev/gomoqt` to v0.20.0 (via v0.18.0, #392) and the playground's `@qumo/moq` to `^0.20.0`.** The relay now speaks moq-lite draft-05, answers `TRACK_INFO` requests, and emits `draft-ietf-moq-msf-01` catalogs (`InitRef` into `InitDataList`); clients on `@qumo/moq` 0.17 can no longer play from it.
- **Hubs dial all remote hubs, not just the first (`internal/relay`).** Removes a single point of failure in cross-cluster links; the remote resolver now sends `?hub=<RELAY_NAME>` as a proper query parameter and logs peer-resolution failures.

### Removed

- **`LocalResolver` and `RemoteResolver` are gone (`internal/relay`).** Peer discovery now uses DNS or static configuration (`UPSTREAM_ADDR` / `PEERS`) instead of control-plane microservices and Nomad API polling.

### Fixed

- **RTSP pull sessions send periodic keepalives (`internal/rtsp`).** Servers that enforce RTSP Session timeouts no longer drop idle TCP-interleaved streams.
- **Docs site corrected where it contradicted the code.** Notably `RELAY_PPROF=0` turns pprof **on** (any non-empty value does), the `RELAY_GOGC` guidance is `600..1600`, `NOMAD_ADDR` is the documented fallback, `qumo update` has its own page, and building from source needs no Deno.

## [v0.6.260906] - 2026-09-06

### Fixed

- **`go install` now ships the real playground web UI instead of a blank page (#376).** If the UI bundles are missing, `qumo playground` now logs a warning and serves an explanatory error page, and the relay and `/config` keep working (`internal/playground`).

## [v0.6.260903] - 2026-09-03

### Added

- **`qumo update` self-update command (`internal/update`).** Replaces the running binary with the latest GitHub release after checking its SHA-256 checksum; `--check` only reports whether an update is available, and dev builds are skipped.
- **`qumo hls` serves a relay track as HLS/DASH over HTTP (`internal/hls`).** Each MoQ group becomes one CMAF fragment stored in qumo-ledger (v0.1.0).
- **`qumo loadgen` reports end-to-end latency.** p50/p95/p99 for the steady-state hold appear in the carry report and in `results.jsonl` (`lat_p50_ms`/`lat_p95_ms`/`lat_p99_ms`/`frames_recv`).

### Changed

- **`MTLS_REQUIRED` now defaults to `true`.** Relays that set `CA_FILE` now reject clients without a cert signed by that CA; set `MTLS_REQUIRED=false` to keep accepting direct browser/WebTransport clients.
- **Go 1.27 is now required to build qumo.** CI's golangci-lint moves to v2.13.2 to match.
- **Dependency bumps: gomoqt v0.17.0 (quic-go v0.61.0) and golang.org/x/crypto v0.55.0.** The x/crypto bump fixes CVE-2026-56854; qumo's own code needs no changes.
- **The Docker runtime image now upgrades its base packages (`docker/Dockerfile`).** This picks up OpenSSL 3.5.8-r0, which fixes CVE-2026-14456.
- **Edges switch hubs less often (`internal/relay`).** A new route must be clearly better on bitrate or RTT before an edge moves, and `qumo_relay_route_replacements_total` now has a `reason` label.

### Removed

- **`ADVERTISE_ADDR` and `INSECURE` are gone.** Neither variable ever changed relay behaviour, so you can delete them from your config; TLS still needs `CERT_FILE`/`KEY_FILE`.

### Fixed

- **The Nomad and Compose demo topologies now actually run hubs and edges.** They passed the role as a `ROLE` environment variable, which the relay ignores; they now pass `--role` on the command line.
- **`qumo rtsp` no longer panics on a malformed broadcast path.** `rtmp`, `rtsp` and `rtsp-push` now reject a path that doesn't start with `/` with a clear error.
- **The playground's RTSP pull server (`:4543`) now only accepts pages from the same host.** It used to accept any WebTransport origin, so any website could open a session to your local ingest (`internal/playground`).

## [v0.5.0] - 2026-07-24

### Release notes — relay performance cycle

- **Relay performance and capacity cycle.** The relay's own code is now a small fraction of CPU under load; most of the remaining cost is in quic-go and the Go runtime (evidence in `docs/perf/`).
- **Measured capacity (WSL2 single host, not a bare-metal claim).** About 13K concurrent subscriber sessions held (about 15K at the establishment peak) and at least 200K objects/sec at 2000 subscribers; bare-metal and distributed-load checks are still open (#341, #342, #343, #344).
- **Opt-in tuning, defaults unchanged.** `RELAY_GOGC` raises the session ceiling on bare metal, `RELAY_UDP_RCVBUF` sets the UDP receive buffer (raise `net.core.rmem_max` on Linux), and `RELAY_PPROF` should be enabled only on a trusted interface.

### Added

- **Opt-in pprof endpoint `RELAY_PPROF=1` (#339, `internal/relay`).** Serves `/debug/pprof/*` on the relay's HTTP mux next to `/metrics`; it is off by default and exposes runtime internals, so enable it only on a trusted or loopback interface.
- **Configurable UDP receive buffer `RELAY_UDP_RCVBUF` (#329, `internal/relay`).** The QUIC socket now uses a 256 KB `SO_RCVBUF` by default so a burst of connecting subscribers does not drop handshake packets; `0` keeps the OS default, and Linux caps the value at `net.core.rmem_max`.
- **Outbound peer re-dials back off exponentially (#305, `internal/relay`).** Re-dials now wait 1s to 30s with jitter instead of a fixed 5s, so many peers dropping at once no longer re-dial together; new metric `qumo_relay_dial_retries_total{peer}` and a `retry_attempt` log field.
- **`qumo loadgen`: out-of-process load generator (`internal/loadgen`).** `qumo loadgen publish` and `qumo loadgen subscribe <N>` drive a separately running relay (`--relay`, `--ca`) and report the relay's own per-session cost from its `/metrics`; `--results <dir>` writes capacity records for the dashboard.
- **`tools/capacity` capacity-sweep driver.** It replaces the removed `qumo loadgen sweep` subcommand and its `--start-relay`, `--relay-cores` and `--gogc` flags, and supports `--sessions`, `--auto`, `--step` and `--bisect`; in `--start-relay` mode each probe gets a fresh relay, and the `capacity-sweep` CI job in `bench-relay.yml` runs it.
- **One-file benchmark dashboard `results/index.html` (`scripts/relay_bench_report.ts`).** It shows the capacity headline, decision summary and every plot offline, and `--paramexp <dir>` adds the paramexp findings.
- **Opt-in GC tuning `RELAY_GOGC` (`internal/gctune`).** Values of 600–1600 reached about 18–20K concurrent sessions on a bare-metal 8-core host, against about 13–15K at the default; it is unset by default, `GOGC` always wins, an invalid value warns and is ignored, and `GOMEMLIMIT` is not used.
- **`qumo doctor` explains the runtime config without changing anything.** It prints the effective GC target, the inputs (`GOGC`, `RELAY_GOGC`, `GOMEMLIMIT`), which one won, and any warnings.
- **`tools/paramexp` measures relay jitter.** The fan-out bench now reports `jitter_ms`.
- **Nightly relay tuning sweep in CI (`bench-relay.yml` `paramexp` job).** It uses `example/relay/params.yaml` and uploads the `relay-paramexp` artifact; workflow inputs `px_samples` and `px_replicates` set its size.
- **`tools/paramexp` Bayesian-optimization scheduler.** Use `--scheduler bo --acquisition ei|ucb|variance`, with yaml knobs `bo_rounds`, `bo_batch`, `bo_acquisition`, `bo_kappa`, `bo_xi`; the report lists suggested next measurements.
- **`tools/paramexp` replicates and variance.** Each vector can run N times (`replicates:` / `--replicates`), and reports show mean ± 95% CI, unstable configs, and the configs that cannot be told apart from the best.
- **The relay fan-out benchmark can be swept by `tools/paramexp`.** Knobs `RELAY_RING`, `RELAY_FRAME`, `RELAY_NOTIFY_TIMEOUT_MS`, plus the `example/relay/{params.yaml,bench.sh}` harness.
- **Automated parameter exploration tool (`tools/paramexp`).** It runs a benchmark over a parameter space (params as `PARAM_<NAME>` env vars, JSON metrics on stdout), stores results in SQLite, and reports knees, parameter importance, interactions and regressions; new dependency `modernc.org/sqlite`.
- **Relay-chain benchmark suite with JSONL results and plots (#284).** It writes results to `BENCH_RESULTS_DIR` and accepts a `FANOUT_KS` override, `scripts/relay_bench_report.ts` draws the plots, `bench-relay.yml` runs it nightly and on demand, and the per-PR gate now skips the `TestRelayChain_*` tests.
- **A publisher that loses a route election takes over when the current one ends (#279, `internal/relay`).** Previously the losing route was discarded, so the path went dark when the incumbent ended; new metrics `qumo_relay_routes_retained`, `qumo_relay_route_promotions_total`.
- **GOAWAY handling (#280, `internal/relay`).** GOAWAY from an upstream peer is now logged and counted (`qumo_relay_peer_goaway_received_total{redirect}`), and on graceful shutdown the relay advertises the `GOAWAY_REDIRECT_URI` env as its redirect.

### Changed

- **Relay metric sampling runs on one goroutine (`internal/relay`).** Metric meanings are unchanged, but the per-address `session_rtt_seconds` histogram series is now deleted when a session ends instead of leaking.
- **`qumo loadgen subscribe` no longer accepts `--ramp` (#327).** All sessions now start at once and spread out through dial backoff; the settle timeout is fixed at 30s.
- **deps: `github.com/qumo-dev/gomoqt` `v0.16.1` → `v0.16.2-0.20260718145816-7bc42f96aec4`.** Each subscriber connection now uses about 2–3 fewer goroutines; `TrackWriter.Updated()` was replaced by `TrackWriter.ReadUpdate()`, which qumo did not use.
- **`tools/paramexp` packages cut from 11 to 7.** `encoding` moved into `experiment`, `provenance` into `storage`, `visualization` into `report`, and `scheduler` into `sampler`.
- **`tools/paramexp` rebuilt as a performance-landscape framework.** It adds a Gaussian-process surrogate with uncertainty, typed parameters (continuous, discrete or categorical), response-surface plots, and run provenance, retries and timeouts; new dependency `gonum.org/v1/gonum`.
- **Broadcast notification cost no longer grows with subscriber count (#332, `internal/relay`).** This also fixes a short delay that could hold back an in-flight frame.
- **Egress byte counter updates once per group (#333, `internal/relay`).** Bytes are still counted when a write fails partway through a group, so metering stays accurate.

### Removed

- **In-process capacity benchmarks `BenchmarkRelay_ConnectionCarry` and `BenchmarkRelay_CapacityFrontier`.** Use `qumo loadgen` to measure the session ceiling instead.
- **Egress poll timer and its `RELAY_NOTIFY_TIMEOUT` / `RELAY_NOTIFY_TIMEOUT_MS` env vars (`internal/relay`).** Delivery never depended on the timer; removing it cuts idle CPU and raised the measured session ceiling from about 4.5K to over 10K.

### Fixed

- **RTSP sessions no longer pile up on a long-lived connection (`internal/ingest`).** Each new ANNOUNCE on the same RTSP connection now closes the previous session instead of keeping it open until the TCP connection ends.
- **A subscriber that hits the peer's stream limit no longer blocks forever (`internal/relay`).** Opening a group now waits at most 30ms and then drops the group; the relay's `MaxIncomingUniStreams` / `MaxIncomingStreams` go back to the quic-go defaults (about 100).
- **`tools/paramexp` statistics and selection fixes (#297, #298).** Suggested points are no longer duplicates or already measured, small discrete spaces no longer end the search early, and confidence intervals now use sample variance and t-values, so they hold at small N.
- **`tools/paramexp` no longer writes an empty `report.json`.** Near-constant data used to produce NaN confidence intervals, which broke the JSON output without any error.
- **`tools/paramexp` builds again (#294 regression).** The default report output directory is now `report_out/`, so the `report` source package is no longer gitignored.
- **`tools/paramexp` model and analysis fixes.** The GP surrogate math is corrected, knees are now found on concave curves, regression results are deterministic, telemetry keys no longer leak into metrics, and `:memory:` storage keeps its pragmas.
- **Subscriber teardown no longer hangs (#286, `internal/relay`).** A subscriber that had fallen behind could ignore cancellation, which hung `Server.Shutdown`/`Close` and leaked goroutines during churn.

## [v0.4.0] - 2026-07-08

### Breaking Changes

- **Relay `ROLE` env var removed; use `qumo relay --role hub|edge` (`internal/relay`).** There is no env fallback, so deployments that set `ROLE=...` must switch to the flag; omit it for a flat single-node relay.
- **SDN controller removed.** The `qumo sdn` subcommand, `internal/sdn`, `internal/topology` and `config.sdn.yaml` are gone; cross-relay discovery now uses moq-lite draft-03 ANNOUNCE_PLEASE, and relay-to-relay links are set via `peers` in `config.relay.yaml`.
- **ALPN changed from `moq-00` to `moq-lite-03`.** Upgrade all peers together; mixed-version deployments are not supported.

### Added

- **RTSP pull ingest: `qumo rtsp <url> [path]` connects IP cameras directly (`internal/rtsp`, `internal/ingest`).** It pulls H.264/AAC over TCP-interleaved RTSP (Basic/Digest auth in the URL, auto-reconnect) and serves MoQT; the old push server is now `qumo rtsp-push`.
- **`qumo playground` one-command local demo (`internal/playground`).** One binary generates a 14-day dev cert, runs the relay in-process and serves the embedded UI on `127.0.0.1:8080`; flags are `--ui-addr` and `--relay-addr`, and the former `VITE_APP_NAME` env var was dropped.
- **Playground UI refresh with an "IP Camera" scenario (`playground`).** Tabs are renamed "Webcam", "IP Camera" (RTSP pull via `POST /api/pull`, `/api/pull/stop`, `/api/pull/status`) and "RTSP Push", plus visual polish.
- **Playground live stats overlay (#139), clearer error messages (#138), encode-quality picks (#135), viewer mute/volume/fullscreen (#136) and a Camera/Screen source toggle (`playground`).**
- **Playground logs through `@okdaichi/media-log` (`playground`).** Logs are structured and level-filtered, with per-second fps/bitrate/RTT/queue lines; requires `@okdaichi/media-log@^0.1.0` on jsr.

### Changed

- **Relay `GROUP_CACHE_SIZE` and `FRAME_CAPACITY` now take effect (`internal/relay`).** They were previously ignored; the example `GROUP_CACHE_SIZE` default is corrected from 100 to 8 to match actual behaviour.
- **Unused relay `REGION` env var removed (`internal/relay`).**
- **Dependency refresh, minor/patch (`go.mod`, `playground`).** `@qumo/moq` 0.16.2, `solid-js` 1.9.14, `@types/node` 25.9, `vite` 7.3.6 and Go indirects.
- **Major frontend bumps (`playground`).** `typescript` 6.0, `vite` 8.1, `@types/node` 26.
- **Relay default `RELAY_ADDR` is now `:4433` (dual-stack IPv4+IPv6) (`internal/relay`, `magefiles`).** `mage relay` also defaults `ADVERTISE_ADDR=localhost:4433` and dev CORS origins so it works with `mage web` out of the box; refs #234.
- **`mage cert` prefers mkcert for a trusted long-lived local cert (`magefiles`).** The new `CERT_HOSTS` env var adds extra SANs; without mkcert it falls back to the 14-day self-signed cert and `VITE_CERT_HASH`; closes #196.
- **Release binaries and the Docker image now embed the real playground UI (`docker`, `.github`).**
- **RTMP/RTSP ingest now forward AVCC video (`avc1`) with Base64 catalog `initData` (`internal/ingest`).** Ingested streams now match the browser-publish wire format and decode in browsers.
- **gomoqt bumped to v0.16.1 (`go.mod`).** It fixes an OOM denial-of-service from unbounded varint allocation, a `Server.Close()` hang and a `Session.CloseWithError` deadlock (#205); `OpenGroup`/`OpenGroupAt` now take a `context.Context`.
- **Web demo directory renamed `solid-deno` → `playground`.**
- **Demo deps bumped (`playground`).** `@okdaichi/av-nodes` 0.10.4, `@okdaichi/golikejs` 0.10.0.

### Removed

- **Bootstrap server and `qumo bootstrap` command removed (`internal/bootstrap`).** This functionality is moving to a separate control-plane service.
- **Stale `examples/web-demo/` removed.** The web demo lives in `playground/`.

### Fixed

- **Browsers can now connect to the relay cross-origin via `CORS_ALLOWED_ORIGINS` (`internal/relay`, `internal/cors`).** The relay and ingest servers share one origin check (same-origin default, `*` opt-out); rejected origins are now logged with a hint.
- **`qumo playground` connects from the browser out of the box (`internal/playground`, `internal/cors`).** It defaults `CORS_ALLOWED_ORIGINS=same-host`, a new port-insensitive allow-list entry; closes #225.
- **Relay no longer panics at startup without `REMOTE_RESOLVER_URL` (`internal/relay`).**
- **RTSP ingest now works with ffmpeg (`internal/ingest`).** It accepts `;unicast;` in the Transport header and matches `MPEG4-GENERIC` case-insensitively.
- **RTSP streams no longer play with a broken picture or audio pops (`internal/ingest`).** Each access unit is now one frame (STAP-A is split), and one packet's AAC units go in one group so they arrive in order.
- **RTSP ingest no longer causes PTS jumps from repeated same-timestamp IDRs (#229) (`internal/ingest`).**
- **Ingested AAC audio now plays in the playground subscriber (`playground`).**
- **Playground `/api/pull` validates the RTSP URL and broadcast path (`internal/playground`).** Do not expose `/api/pull` in a publicly hosted deployment.
- **`qumo playground` refuses a cached cert valid longer than 14 days (`internal/playground`).** Unset `QUMO_PLAYGROUND_CERT_DIR` or use a cert valid for 14 days or less.
- **`qumo version` no longer reports a `-dirty` suffix on clean builds (`magefiles`).**
- **Playground fixes (`playground`).** The production UI build works again, a subscribe to an empty path says "No stream at this path yet" instead of a connection failure, mkcert users get no false cert warning, and the `mage web` console is quieter.

### Security

- **RTMP listener resists handshake stalls (`internal/rtmp`).** A stalled or bad client now times out (10s default) and is skipped instead of blocking or crashing the ingest server.
- **`golang.org/x/crypto` bumped to v0.53.0 (`go.mod`).** This clears CVE-2026-39829, -39830, -39832, -39835, -42508, -46595 and -46597; `golang.org/x/sys` is now v0.46.0 and `golang.org/x/text` v0.38.0.
- **Ingest servers check WebTransport origins against `CORS_ALLOWED_ORIGINS` (`internal/ingest`).** This closes a cross-site request forgery risk; `*` and a same-origin fallback are supported.
- **TLS configuration hardened (`internal/relay`):** Removed `InsecureSkipVerify` from the relay dialer, so outgoing relay connections verify TLS.
