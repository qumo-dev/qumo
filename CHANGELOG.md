# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [v0.13.261009] - 2026-10-09

> **Breaking for Go callers and funnel senders.** `token.Grant` is `{Scopes, Subject}`: its `Publish` and `Subscribe` fields are gone, and `token.Sign` writes a `scopes` claim, never `path_auth`. A funnel no longer infers a sender from a credential publishing one segment beneath the broadcast: such a credential no longer records into it, and the sender is the credential's `sub`. See **Changed** below.

> **Behavior change for apps.** A session no longer ends when its credential expires. An app ends a session by withholding its next credential and, at once, by removing the key from the set. See **Changed** below.

### Added

- **Credentials grant actions on exact tracks, and name their bearer (`token`, `internal/auth`).**
  - **`scopes`.** A token grants a `scopes` claim, the JSON counterpart of CAT-4-MOQT's `moqt` claim: each scope lists `actions` (`publish`, `subscribe`, `fetch`), a `broadcast` match (`{"exact": "room/123"}`, or `{"prefix": "room/123"}` for it and every path beneath it on `/` boundaries, within the key's prefix) and an optional `track` match (`{"exact": "chat"}`; omitted, every track). Whatever no scope grants is denied. An unknown action or scope member, an empty scope list, or a token carrying both `path_auth` and `scopes` is refused.
  - **`sub`.** A token with `scopes` may name its bearer in `sub`.
  - **Go.** `token.Scope` and its `Allows`, `auth.Grant.Allows` (may this action reach this broadcast and track), `auth.Grant.Announces` and `auth.Grant.Subject`.
  - **`qumo auth token -scope ACTIONS:BROADCAST[:TRACK]`** (repeatable; `a/b/**` for a prefix) and **`-sub`**.

### Changed

- **One grant model: scopes (`token`, `internal/auth`, `internal/funnel`, `internal/relay`).**
  - **`token.Grant`** is `{Scopes []Scope; Subject string}`. `Sign` writes only `scopes` (and `sub` when there is a subject).
  - **`path_auth` is still accepted** from tokens signed elsewhere (the `@moq/token` convention), and read as scopes: `pub` as `publish`, and `sub` as `subscribe` and `fetch`, each on its path and every path beneath it, on every track. `sub` (the claim) with `path_auth` is still refused.
  - **`qumo auth token -publish PATH` / `-subscribe PATH`** stay, as short for `-scope publish:PATH/**` and `-scope subscribe,fetch:PATH/**`.
  - **The relay** lets a session announce a broadcast a `publish` scope matches on every track (one naming no `track`), and subscribe to a track a `subscribe` scope matches, by broadcast and track name. There is no separate announce action. A broadcast has one publisher: a `publish` scope naming one track doesn't announce, since it writes that track into a broadcast someone else announces, such as a funnel's, and so can't take the broadcast's route from its publisher. It ignores `fetch`. An internal client's grant is in the same model: a `subscribe` and `fetch` scope prefixing no path, which reaches every broadcast.
  - **`qumo funnel`.** Recording into a track needs a `publish` scope matching its broadcast and name, and records as the credential's `sub`, or with no sender without one; reading history needs a `fetch` scope matching them. A path never names a sender. With `RELAY_SIGNING_KEY`, the funnel signs its relay credential as a `publish` scope on `RELAY_PUBLISH` and beneath it.
  - **Key sets and usage.** A session publishes a broadcast when a `publish` scope permits every track of one. A key marked `"pause": ["publish"]` starts no such session, and usage reports count only such sessions as publishers: a viewer whose `publish` scope names one track, to write it at a funnel, still connects and is counted with the viewers.
  - **Upgrading.** Upgrade relays and funnels before the apps that sign tokens: `token.Sign` writes `scopes`, which an older relay refuses as an unknown claim.
- **Behavior change: a credential's expiry decides whether a session may start, not how long it lives (`token`, `internal/auth`).**
  - **At connect, nothing changes:** `exp`, `nbf` and `iat` are checked with the 60 s leeway, and an expired credential is refused.
  - **A live session outlives its credential's expiry,** so an app can issue short-lived credentials without its clients reconnecting, and dropping audio, every time one expires. The relay no longer ends a session with reason `expired`, and `qumo_relay_sessions_ended_total{reason}` counts only `refused`.
  - **The key set still governs live sessions.** A key that leaves the set ends every session it admitted at the next 30 s re-check; `"pause": ["publish"]` still refuses new sessions that may publish and lets live ones continue.
  - **`reval` is refused,** as any claim the relay doesn't know: CAT's `moqt-reval` asks for a live session to be revalidated against its token's expiry, which never happens here.
  - **Go.** The new `token.VerifyLive` checks a live session's credential as the relay does: as `token.Verify`, without refusing it for its expiry.
  - **Usage reports:** a session whose end never comes is forgotten five minutes after it was last re-checked, no longer five minutes after its credential's expiry.
- **A key in a key set pauses its new publishing sessions with `"pause": ["publish"]` (`internal/auth/keyset.go`).** The member names what it does: the key starts no new sessions that may publish, while viewers still connect and live sessions continue. `"publish"` is the only entry; any other entry in `pause` makes the set invalid, so a misspelled entry can't leave a key unpaused, and the relay keeps its last good set as with any invalid set. That covers entries only: member names are case-sensitive, and a member the relay doesn't know (`"paused"`, `"Pause"`) is ignored, as JWK prescribes. A key naming a member twice, or carrying `"publish": true` beside `"pause": ["publish"]`, also makes the set invalid; key sets are now decoded with `encoding/json/v2`, as in the `token` package, so the relay and `token.ParseKeySet` read a set the same way. A key set without a `"keys"` list (`{}`, `{"keys": null}`), which was read as an empty set, is now refused, so the relay keeps its last good set instead of dropping every key and ending every live session; `{"keys": []}` is still a valid empty set.

### Deprecated

- **`"publish": false` on a key** still pauses its publishing, but reads as if the key could never publish. Write `"pause": ["publish"]`; the old member will be removed in a later release. The relay logs a warning naming the keys that use it when it loads such a set, once per change rather than on every refresh.

### Fixed

- **qumo-ledger moves to v0.2.2 (`qumo funnel`, `qumo hls`).** Seeking a track by media time or wall-clock time now reports a store that can't be read as an error, not as a missing group; a group ID no longer loses the bits of an epoch or sequence too wide for it; and a fragment whose `trun` is shorter than its flags declare is refused rather than given any duration.

### Security

- **qumo builds with Go 1.27.2 and `golang.org/x/net` v0.60.0, which fix GO-2026-6603 to GO-2026-6617** (`net/http`, its internal HTTP/2, `crypto/tls`, `net/textproto`, `os`). The relay's WebTransport and HTTP paths call the affected code. `go.mod`'s `go` directive is `1.27.2`; `x/crypto`, `x/sys` and `x/text` moved with `x/net`. CI lints with golangci-lint v2.14.0, built with that toolchain (`install-mode: goinstall`): v2.13 can't read Go 1.27.2's export data.
- **`CA_FILE` is read through `os.Root`** on the working directory, so a symlink can't lead it outside, as a `..` already couldn't. golangci-lint v2.14.0's gosec flags the plain read as path traversal.

## [v0.12.261008] - 2026-10-08

> **Breaking for operators.** Relay peers authenticate with mutual TLS under a relay CA: a relay no longer presents `CERT_FILE` to the relays it dials, and `PEERS` needs `PEER_CERT_FILE`, `PEER_KEY_FILE` and `CA_FILE`. To move over, issue a peer certificate per relay from your CA and set the two new variables. See **Changed** below.

### Added

- **`qumo funnel`: funnels many HTTP senders into one MoQT track, recording each record before it is sent (`internal/funnel`).**
  Many senders POST records into one track, and a subscriber receives all of them on one subscription. The funnel publishes through a relay it dials as a client, and can also serve subscribers itself; it does not join the relay mesh as a peer.
  - **Requests.** A record is the body of `POST /tracks/room/123/chat` — the track `chat` of the broadcast `/room/123` — one JSON value in UTF-8; it creates the track when it does not exist. `PUT` to the same URL creates the track ahead of its first record, and `GET` answers a page of its history, oldest first: the newest records, or those `?before=` a group, at most `?limit=`.
  - **Records.** Each is stored and sent as `{"sender": …, "payload": …}`: the payload as posted, and the sender its credential names.
  - **Retries and limits.** A record with an `Idempotency-Key` header is stored once per sender and track; a retry with the same key gets the first reply. `FUNNEL_SENDER_LIMIT` and `FUNNEL_TRACK_LIMIT` (`RATE,BURST`) limit records per sender and per track, answering `429` with `Retry-After`.
  - **Credentials.** With `QUMO_AUTH_KEYS`, every request carries a qumo credential as `Authorization: Bearer`, verified against the relay's key set. Reading needs a grant to subscribe at the broadcast path. Recording needs a grant to publish at the broadcast path, which records with no sender, or one segment beneath it, which names the sender: `/room/123/alice` records into `/room/123` as `alice`. It is checked on every request, so an expired or revoked credential is refused at once. A `401` carries `WWW-Authenticate: Bearer`. Without a key set, every request is accepted with no sender.
  - **Through a relay.** With `RELAY_URL`, the funnel dials the relay as a client and announces its broadcasts there, so subscribers use the relay they already reach. Its credential is the `?jwt=` in the URL, or, with `RELAY_SIGNING_KEY` and `RELAY_PUBLISH`, one it signs afresh for every session. When the session ends, as the relay ends it when its credential expires, the funnel dials again. Its own MoQT listener (`FUNNEL_SERVE_ADDR`) is then off unless set.
  - **Recorded, then sent.** Each record is committed to a qumo-ledger track through qumo-ledger's new `ingest` package, and only then sent to MoQT subscribers, in commit order.
  - **Where records go.** `LEDGER_URI` names the store, and its scheme selects one of qumo-ledger's backends: `file:///var/lib/qumo` is a directory, `postgres://…` a table in PostgreSQL or CockroachDB, `s3://bucket/prefix?region=…` an S3 or S3-compatible bucket, and unset or empty is memory, lost on exit and not bounded. A bare path or any other scheme is an error.
  - **On the wire.** One record is one group holding one frame, the record as stored. A group's sequence is one more than the sequence of the ledger group that stores the record. A new subscriber starts at the track's latest record. Only created tracks are served; another track name is refused as not found.
  - **Restarts.** At startup the funnel lists its store and publishes every track it recorded before, with its latest record, so subscribers reach them before anyone records again. Idempotency keys do not survive a restart.
  - **Configuration.** `FUNNEL_ADDR` (default `:8090`), `RELAY_URL`, `RELAY_SIGNING_KEY` / `RELAY_PUBLISH`, `RELAY_CA_FILE`, `RELAY_TLS_INSECURE`, `FUNNEL_SERVE_ADDR` (default `:4433` without a relay), `LEDGER_URI` (default memory), `CERT_FILE` / `KEY_FILE`, `CORS_ALLOWED_ORIGINS`, which also governs browser requests, and `QUMO_AUTH_KEYS` / `QUMO_AUTH_KEYS_CACHE` / `QUMO_RELAY_TOKEN` as the relay reads them, and `FUNNEL_SENDER_LIMIT` / `FUNNEL_TRACK_LIMIT`.
  - **Limits.** The HTTP listener is plain HTTP; terminate TLS in front of it. Records of a track are ordered within one process only, so one funnel writes a track. A track idle for 10 minutes is closed and opened again from the store on its next record. Recorded tracks carry no duration and are not served by `qumo hls`.
  - **Dependency.** qumo-ledger moves to v0.2.0, which adds the `ingest` package and `store.Open`.

### Changed

- **Breaking: relay peers authenticate with certificates a relay CA issued, in both directions, and a relay's peer identity is separate from its public certificate (`internal/relay/peer_trust.go`).**
  - **A relay no longer presents `CERT_FILE` to the relays it dials.** Its peer identity is `PEER_CERT_FILE` and `PEER_KEY_FILE`: a certificate `CA_FILE` issued, carrying the DNS name `peer.qumo.internal` and usable for client and server authentication. The subject common name is the relay's identity for logs; the relay never interprets it.
  - **The dialed relay is verified against the relay CA too.** A dialing relay asks for the server name `peer.qumo.internal`; the dialed relay answers it with its peer certificate, and the dialer verifies that against `CA_FILE` alone. Public certificates play no part in relay identity, and `PEERS` may name peers by any address (a Consul name, an IP).
  - **A CA-issued certificate authenticates; only the peering name makes a peer.** A session whose certificate `CA_FILE` verifies but lacks the name is an internal client, such as the HLS egress: it may subscribe to anything and announce nothing. One carrying the name is a relay peer, served without a credential.
  - **Strict settings.** `PEER_CERT_FILE` and `PEER_KEY_FILE` are set together and need `CA_FILE`; `PEERS` needs all three; a peer certificate the CA didn't issue, without the name, or expired stops the relay at startup. `CA_FILE` alone remains valid and authenticates internal clients. There is no fallback to `CERT_FILE`: a relay without the two settings has no peer identity.
  - **A relay recognizes itself.** With `PEERS` naming a group that resolves to the relay too, it sees its own certificate on that session, drops it and doesn't retry. `PEERS` is still resolved once at startup; a relay that starts later must dial the earlier ones.
  - **Only native-QUIC clients are asked for a certificate.** A browser never is.
  - **What the relay asks of the certificate files.** One peer certificate per relay: a relay recognizes itself by its own, so two relays sharing one never peer. A certificate issued by an intermediate CA works with the intermediate after the leaf in `PEER_CERT_FILE` and the root in `CA_FILE`. The files are read once at startup, so a renewed certificate needs a restart.
  - **To move over:** issue a certificate per relay from your CA as `docs/site/content/en/docs/deployment/tls.md` describes (an OpenSSL example is there), and set the two new variables. For development, `mage cert` now also writes a relay CA and peer certificates (`PEER_NAMES=a,b mage cert`); the Compose topologies, the Nomad simulation and the multi-process benchmark use them.

### Fixed

- **An RTSP pull no longer drops a healthy stream on the server's answer to a keepalive (`internal/rtsp`, `internal/ingest`).** The keepalive added to stop servers timing a session out was read back as if it were a request, so several ordinary answers ended the pull and forced a reconnect every 5 to 30 seconds:
  - **Any answer with a reason of more than one word** ("405 Method Not Allowed", "454 Session Not Found") failed to parse. Responses are now read as responses.
  - **Any answer with a body** left the body on the connection, where it was taken for the next message. It is now read to its end.
  - **A camera that does not implement `GET_PARAMETER`** (405 or 501) is kept alive with `OPTIONS` from then on.
  - **A camera that authenticates every request** answered the keepalive with 401 and the session was never refreshed. Keepalives now carry the credentials once the server has challenged.
  - **454 Session Not Found** ends the pull with `rtsp.ErrSessionNotFound`, so it reconnects at once and not when the stream dries up.
  - **A peer that stops reading** can no longer hold a keepalive, and with it the close of the connection, for ever: the write gives up after 5 seconds. Stopping the keepalive waits for it to finish, and a keepalive that fails is logged as a warning and tried again at the next interval.
  - **A data race** on the request sequence number between the keepalive and the close is gone.
- **A credential whose header carries `crit` is refused** (`token.Verify`, and so the relay). `crit` lists headers a verifier must understand (RFC 7515 4.1.11); the relay understands none beyond `alg`, `kid` and `typ`, and took such a token as valid. No token `token.Sign` or `qumo auth token` makes carries one.
- **Every answer of the HLS egress carries `Vary: Origin`** (`internal/hls/cors.go`), not only one to an allowed origin. A manifest or segment fetched with no `Origin`, or from an origin that isn't allowed, was answered without it, so a shared cache or CDN in front of the egress could keep that answer and serve it to a page on an allowed origin, whose browser then refused it.

### Removed

- **`trackBuffer.SetFrameCleaner` (`internal/ingest`), added for frame pooling to come.** Nothing called the callback it stored, and its comment said the egress loop did. Called as described, it would have handed a frame back to a pool while other subscribers of the same track were still being sent it. It returns with the pool, when there is a point at which a frame is known to be done with.

## [v0.11.261005] - 2026-10-05

> **Breaking for operators.** The auth server is removed: the relay no longer asks one (`QUMO_AUTH_URL`), and `qumo auth` no longer runs one. The relay verifies credentials itself against a key set. To move over, unset `QUMO_AUTH_URL`, set `QUMO_AUTH_KEYS` to the key set the auth server was reading, and stop the `qumo auth` process; tokens and signing keys are unchanged. A relay with `QUMO_AUTH_URL` still set refuses to start. See **Removed** below.

### Added

- **The playground's DevTools timeline can be zoomed and moved (`playground/src/devtools/view.ts`).** It showed the last 60, 10 or 2 seconds and nothing in between, always up to the present.
  - **Zoom:** scroll on the timeline, or press − and +, for any span from half a second to the full minute that is kept. The 60 s, 10 s and 2 s buttons remain as shortcuts, and the span in view is shown next to them.
  - **Move:** drag the timeline to look further back. A moved view stays on the moment it was put on while new media keeps arriving, so something that just happened can be looked at without pausing. Zooming a moved view keeps what is under the pointer under the pointer.
  - **Live:** returns to the present and follows it again. A live view zooms from the present and stays live.
  - **The axis** shows the times of what is in view, to the tenth of a second when little is.
- **The playground can publish a test pattern, with no camera (`playground/src/publish/pattern.ts`).** "Test pattern" is a third source next to Camera and Screen: colour bars, the time of day to the millisecond, a frame counter and a marker that crosses the picture once a second, with a 440 Hz tone. Each second the picture flashes as the tone beeps, so sound and picture can be checked against each other, and the clock can be read off two screens for the delay. It needs no permission, is exactly the size and frame rate chosen, and keeps its frame rate in a background tab. The frame count, the flash and the marker are all read off the clock, so a timer that runs fast or slow cannot shift them.
- **The relay verifies credentials itself, against a key set (`QUMO_AUTH_KEYS`).** No other process is needed: `QUMO_AUTH_KEYS=keys.json qumo relay` with the key set `qumo auth keygen` writes. The value's form says where the set is: an `https://` URL (or `http://` on a loopback host) is downloaded, a path or `file://` URL is read, and any other scheme is refused at startup.
  - **The checks** are the `token` package's: a known `kid`, the EdDSA signature, exactly the allowed claims, the times (60 s leeway, at most an hour), and every granted path within the key's `prefix`. A session may publish and subscribe where its token says and ends when the token expires.
  - **The key set** is a file, re-read when it changes, or a URL, downloaded every 30 s (±10% jitter, so relays restarted together don't poll in step) with `If-None-Match` and `QUMO_RELAY_TOKEN` as a bearer token (https, or http on a loopback host).
  - **Live sessions are re-checked every 30 s:** one whose key has left the set ends with `0x2` (Unauthorized), so removing a key cuts its sessions off within about a minute.
  - **Fail-static:** a failed refresh keeps the last set; after 6 h without one, new sessions are refused while live ones run to their expiry. Nothing is admitted before the first load.
  - **Usage reports (`QUMO_USAGE_URL`):** each verified session's open, its cumulative bytes every 30 s, and its close with the final totals and reason, POSTed as JSON every 10 s with the same bearer token. Usage is coalesced per session and sent in batches of at most 500. A failed send, a 401, 403, 408, 413 or 429 included, keeps the records for the next try; only a batch the receiver can't read (400 or 422) is dropped. At shutdown the relay waits up to 5 s for its last sessions to record their end before the final send.
  - `qumo auth keygen`'s next steps now point the relay at the key set directly.
  - **Viewers are reported together.** Sessions that only subscribe no longer send a record each: their bytes are added up per key into one running total per relay run (`session_id` `viewers.<run>.<kid>`), so a usage receiver's load follows the number of keys, not the size of the audience. Sessions that may publish are still reported one by one.
  - **`"publish": false`** on a key starts no new sessions that may publish, while viewers still connect and live sessions continue: for a limit on broadcasts that must not lock the audience out.
  - **`QUMO_AUTH_KEYS_CACHE`:** a file the last downloaded key set is kept in and loaded from at startup, so a relay restarted while the key-set URL is unreachable still admits sessions. The file's age counts toward the 6 h limit.
  - **A key set file that can't be read stops the relay at startup.** It used to start and refuse every session.
  - **A usage outage is logged once,** and once more when sends recover, not every 10 s.

- **The playground's DevTools panel says how playback is going, and keeps a log in words (`playground/src/devtools/log.ts`).**
  - **Status:** one line at the top, "Playing normally" or what is wrong and why, from the last ten seconds.
  - **Log:** what happened, each with its time: the delay set or raised, groups skipped, aborted or late, sound lost and why, the page stopping, nothing arriving. It can be copied as text.
  - **Incidents:** trouble that came together is one item, headed by its likely cause and what it cost ("Nothing arrived for up to 463 ms: the audio buffer ran dry, 400 ms of sound lost, delay raised to 500 ms"), with its entries underneath. It reads the same a minute later as it did live.
  - **Log and timeline are tied together:** pointing at a log item marks its time across the timeline, and the timeline's axis shows clock times, the same ones the log uses.
  - **Readable without the source:** the playback delay, arrival jitter and audio buffer level stay in view and the other counters move under "More figures"; every label, table heading and legend entry explains itself when pointed at.
  - **One word for one thing:** the table, the legend, the log, the figures and the text shown when pointing at a mark use the same names ("Received", "Played", "Page stopped", "Nothing arriving"). Pointing at a mark on the timeline shows what it is, in a sentence, and the clock time it happened, in the panel's own box.
  - **A legend that can be looked up:** one row per kind of timeline row, named as on the timeline, and no two marks on a row look alike. "Stopped" is hatched grey and "Waited" a solid strip; they used to look like "Received" and "Skipped".
  - **Three lanes, not four:** the page's stops are bands across every lane, since nothing in any of them moves meanwhile, in place of a "main thread" lane that was nearly always empty. "audio out" is named "audio buffer", which is what it shows.

### Fixed

- **The playground's DevTools count the page's stops in a background tab too (`playground/src/devtools/stall_monitor.ts`).** The watch ran on the page's own timer, which the browser slows to once a second in a background tab, so stops there were left out rather than miscounted. It now takes its ticks from a worker and measures how long each waited for the main thread, which is the same in view and out of it. A ticker that itself pauses, as in a frozen tab, reads as no stop. The worker timer is shared with the test pattern (`playground/src/worker_ticker.ts`).
- **The playground dials the relay on the port it was started on, and looks for the HLS egress on the host the page was opened at.** Two leftovers of #456, for a playground opened anywhere but `localhost:4433`:
  - **Relay port:** `qumo playground --relay-addr` with a port other than 4433 served that port in `/config`, but the Webcam and HLS scenarios dialled 4433 regardless. They now take the port from `/config`. The ingest scenarios keep their own ports.
  - **HLS egress:** the HLS scenario fetched its playlist from `http://localhost:8081` whatever host the page was opened at. It now uses the page's own host at port 8081: over http on this machine, and over https from an https page anywhere else, which holds only if something in front of the egress terminates TLS on that port. `VITE_HLS_URL` still replaces that under `mage web`.
  - **Under `mage web`,** a `VITE_RELAY_URL` that is not an https URL falls back to `https://localhost:4433` where it used to leave the page connecting for ever, and a camera pull started before the config has been read is dialled once it has.
- **A refused native-QUIC session is closed before its subscribes are answered**, so a client learns `unauthorized` rather than `track does not exist` for a path that may well exist; `qumo hls` reports the refusal and the two ways in (a credential in `RELAY_URL`, or `RELAY_CERT_FILE` from the relay's CA) instead of waiting for a publisher that was never the problem.
- **The ingest no longer busy-loops in the moment between a new group being counted and being stored (`internal/ingest`).** A subscriber that looked in that moment asked for the group again in a tight loop until it appeared; the moment is normally a few instructions long, but lasts as long as the pushing goroutine is held up. It now waits for the notification that follows every push, as it does when there is nothing new at all. The step that picks a subscriber's next group is its own function, with tests for falling behind and for a group that is counted but not yet stored.
- **A playground opened at a host other than localhost dials that host, not localhost (#456).** `qumo playground` already answered `/config` with the relay URL for the host the UI was opened at, but the UI took only the cert hash from it and built the address from a build-time variable that is unset in the shipped bundle. The relay address and the ffmpeg push command shown for RTMP and RTSP now use the host from `/config`. Under `mage web` they use `VITE_RELAY_URL`'s host, as before. The HLS scenario still reads `VITE_HLS_URL`, since `/config` does not name the HLS egress.
- **The playground's viewer no longer drops seconds of audio as it starts.** The audio output is started before the audio track is subscribed to: starting it can take seconds the first time, and everything that arrived meanwhile was handed over at once and thrown away. A backlog passed over before anything has played is no longer counted as lost audio, and silence before playback begins is no longer counted as starvation.
- **Raising the playback delay interrupts the sound less often.** Every raise holds playback while the buffer fills, and the delay used to creep up a few milliseconds at a time, a short silence each. It now moves only for a raise of 10 ms or more, and then a little further.

### Removed

- **Breaking: the auth server is gone: the relay no longer asks one (`QUMO_AUTH_URL`), and `qumo auth` no longer runs one.** The relay verifies credentials itself against a key set (`QUMO_AUTH_KEYS`), which does the same checks in the relay process.
  - **To move over:** unset `QUMO_AUTH_URL` on the relay, set `QUMO_AUTH_KEYS` to the key set the auth server was reading, and stop the `qumo auth` process. Tokens and signing keys are unchanged.
  - **A relay with `QUMO_AUTH_URL` still set refuses to start,** rather than starting with auth off.
  - `qumo auth` needs a command (`keygen` or `token`); `QUMO_AUTH_ADDR` and `QUMO_AUTH_KEYS_FILE` are no longer read.
  - Metrics: the `invalid` result of `qumo_relay_auth_requests_total` and the `invalid` reason of `qumo_relay_sessions_ended_total` are gone; only an auth server's reply could cause them.

### Changed

- **The playground's boards share their preview and stats overlay, and its colours all come from tokens (`playground/src/components`).** `PreviewCanvas` and `StatsOverlay` replace the markup the publish board, the subscribe board and the HLS player each repeated. The colours that were written into the stylesheet and into inline styles (the black behind a video, the overlay, the text on the Start and Stop buttons) are tokens on `:root`. Conditional and repeated markup uses `<Show>` and `<For>`. The one visible change: the border around a video takes the theme's border colour, where an inline style had made it light grey in both themes.
- **The playground's publish board captures and encodes by itself; `@okdaichi/av-nodes` is no longer a dependency (`playground/src/publish`).**
  - **A publisher with no UI:** `Publisher` takes a media stream and a path, and does the rest: capture, encode, catalog, and a group per GOP (video) or per frame (audio) to each subscriber. `PublishBoard` is the controls around it.
  - **The encoder is sized from the first captured frame,** the one place the true picture size is known. This replaces grabbing a still through `ImageCapture` before starting.
  - **Video and audio are stamped from one clock** that starts with the run. Each keeps its source's own spacing (the camera's frame times, the count of audio samples), so timestamps do not carry the jitter of arrival.
  - **Audio reaches the page in 20 ms blocks,** gathered on the audio thread: 50 messages a second through the main thread where there were 375.
  - **Each subscriber is written to in order, and independently.** A frame is not written to a group before that group has opened, which could happen when a keyframe and the frame after it were encoded close together. A subscriber that falls more than 64 frames behind has frames dropped up to the next keyframe, and the group they were dropped from is recorded as aborted.
  - **A subscriber that leaves is let go of at once,** not at the end of the run.
  - **Stopping releases the microphone** as well as the camera, and a share ended from the browser's own controls stops the run.
- **The playground's viewer decodes and plays by itself; it no longer uses `@okdaichi/av-nodes` (`playground/src/player/audio`, `playground/src/player/video`).**
  - **Audio** is decoded with WebCodecs and played through the viewer's own jitter buffer, a ring of PCM indexed by media time on the audio thread. It holds playback back until the playback delay is buffered, and holds back again after running dry instead of stuttering on an empty buffer. Blocks whose timestamps are a few samples off the end of the last one (RTMP timestamps are whole milliseconds) are joined to it rather than leaving a hole or an overlap at every block.
  - **The playback delay follows how the audio arrives.** It is sized from the measured arrival jitter (how late audio arrives against its fastest arrival), and raised by half if the buffer runs dry anyway, up to 500 ms; it stays raised for the rest of the run. An audio group that is late is waited for only 40% of the delay, since giving up one frame costs far less than emptying the buffer waiting for it. The buffer also returns to its target after a burst instead of staying full, and skips audio it has held for two seconds without needing.
  - **A lost audio frame no longer shortens the buffer for good.** The browser's audio decoder stamps its output by counting samples and ignores later input timestamps, so decoded audio closed up around any frame that was not fed to it, and each one left the buffer a frame shorter until it ran dry. The viewer now notices jumps in the input timestamps and puts them back, so a lost frame is one frame of silence in its own place.
  - **Video** is decoded with WebCodecs, held until due, and drawn on the canvas. The DevTools "rendered" lane now records frames actually drawn, not frames passed on to be drawn.
  - **Why:** how long to buffer and when to play are the player's decisions, and they lived half in the viewer and half in a package this repository does not control.
  - The ring buffer follows the moq-dev reference player's and has unit tests.
- **The playground's DevTools panel shows playback timing and the audio output, and is drawn on canvas (`playground/src/devtools`).**
  - **Timing:** the playback delay and the arrival jitter of audio and video, next to the media bitrate.
  - **Drawn on canvas.** Building an SVG element per mark held the main thread for 50-63 ms every second, which starved the audio the panel was showing.
  - **Audio output:** a row with the jitter buffer's level and how much sound it has lost (ran dry, missing, too late, overflowed, trimmed), and a timeline lane with the level over time and a mark at each moment sound was lost. The lanes above show what the network delivered; this one shows what reached the speaker.

## [v0.10.261005] - 2026-10-05

> **Breaking for operators.** `qumo_relay_sessions_expired_total` is replaced by `qumo_relay_sessions_ended_total{reason}` (use `reason="expired"` for the old count). Sessions are now revalidated with the auth server, which also receives each session's bytes and an `end` report. The HLS egress can connect as a trusted peer with a client certificate. New: `qumo auth`, a ready-to-run auth server with a key generator and token tool, and the `token` package apps sign with. WebTransport clients now see why the relay ended their session. **Not yet enforced:** which paths a session can discover through announce interest and TRACK_INFO (#450).

### Added

- **`qumo auth`: a ready-to-run auth server, key generator and token tool (#460).** A relay with `QUMO_AUTH_URL` asks an auth server about every session; until now qumo shipped none, so a relay ran either with auth off or against an auth server you wrote yourself. An app now decides only what each client may do, and signs it:
  - **`qumo auth keygen [-prefix acme/app]`** writes an Ed25519 signing key (private JWK; it stays on the app's server) and the public key set the server trusts. The `kid` is the key's RFC 7638 thumbprint. It prints the kid, the paths the key may grant and the next steps, and never overwrites an existing key.
  - **`qumo auth`** runs the server that answers the relay (`QUMO_AUTH_KEYS_FILE`; `QUMO_AUTH_ADDR`, default `127.0.0.1:4440`). A session without a token is refused. It checks, in order:
    1. a trusted `kid` and EdDSA;
    2. the signature;
    3. exactly the allowed claims (`path_auth`, `iat`, `nbf`, `exp`, an optional `jti`);
    4. the time claims, with 60 s leeway and at most a one-hour lifetime;
    5. every granted path within the key's prefix.

    It answers with the grant's subtree patterns and `expires`: 401 for a token it can't accept, 403 for one that grants a path outside its key's prefix. It prints a startup banner and logs each admitted, refused and ended session; the token is never logged.
  - **`qumo auth token -publish … -subscribe … -ttl …`** signs a token by hand, for testing: the token on stdout, what it grants on stderr.
  - **The Go package `github.com/qumo-dev/qumo/token`** is what an app's backend signs with: `token.Sign(key, token.Grant{Publish: …, Subscribe: …}, time.Hour)`. It refuses to sign a path outside the key's prefix, and it is standard-library only.

- **Sessions are revalidated with the auth server (#419).** At the grant's `revalidate` cadence, the relay sends the session's connect request again as `event: "revalidate"`, with the same `id`. This is how key revocation, project suspension and spend limits reach live sessions; the auth server decides, and the relay doesn't know which it was.
  - **A 401 or 403** ends the session with `0x2` (Unauthorized) and reason `refused`.
  - **A grant that can't be enforced** ends it with reason `invalid`.
  - **An admitted grant** keeps the session and moves its `expires` and cadence. Its patterns aren't compared, since they were fixed at connect. A grant without `expires` keeps the deadline the session has: a revalidate never lifts it.
  - **No answer** (a timeout or a 5xx) is retried with jittered exponential backoff, from about 1 s up to 30 s. The session lives until its current `expires`, and a stalled request is cut off at that deadline.
  - **One timer per session** drives both expiry (#423) and revalidation, and is stopped when the session ends first.
- **Session bytes and end reports (#424).** The auth server now sees each checked session's usage, so it can attribute it for billing and count live sessions.
  - **Every revalidate** carries the session's cumulative `bytes` (`sent` and `received`, from the relay's side), left out while both are zero. Sending them on revalidate as well as end is qumo's one extension of moq-auth, so billing sees a long session before it ends.
  - **When a checked session closes,** the relay sends `event: "end"` with the connect request's `id`, the final `bytes`, `duration` in whole seconds and a `reason`: `expired`, `refused`, `invalid`, `closed`, `dropped` (an idle timeout or stateless reset) or `upgrade_failed`.
  - **Every `connect` gets an `end`,** so the auth server's live-session count can't be inflated. A WebTransport session admitted at connect whose upgrade then fails reports `end` with reason `upgrade_failed` and no `bytes`. A request from an `Origin` the relay refuses is no longer sent to the auth server at all.
  - **The end report is best effort:** one attempt with a 2 s timeout, sent after the close, so a slow or failing auth server can't hold a session open. A lost one costs at most one revalidate interval of usage.
  - `qumo_relay_auth_requests_total{event="end"}` counts reports, with `result` `ok` or `error`.
- **The HLS egress connects as a trusted peer (#432, ADR 0035 Decision 7).** It is an HLS origin, and viewers are authorized in front of it, so it holds no credential.
  - `RELAY_CERT_FILE` and `RELAY_KEY_FILE` set its client certificate from the private CA the relay trusts as `CA_FILE`. The relay then never asks its auth server about it, and the session never expires.
  - Both settings need a `moqt://` `RELAY_URL`, since only native-QUIC sessions can be trusted peers; the egress refuses to start otherwise.
  - The certificate is read again on every reconnect, so a renewed one is picked up without a restart.
- **`qumo loadgen --relay` takes a `moqt://` URL with a credential** (`moqt://host:port/path?jwt=…`) as well as `host:port`, to load a relay with an auth server. `smoketest`'s `-pub` and `-sub` URLs carry one the same way. Neither refreshes it: they run for less than a credential's lifetime.
- **The playground has a DevTools panel for the subscribed tracks (`playground/src/devtools`).** It sits under the boards, closed until opened, and remembers that choice.
  - **Session:** the media bitrate being received, plus the connection's round-trip time, byte counts and estimated send rate where the browser reports them.
  - **Tracks:** per track, the received bitrate and frame rate, the latest group, and how many groups ended complete, skipped, aborted or late.
  - **Timeline:** the last minute of each track. Groups are drawn as they arrived, one lane unless they overlap, marked by how they ended (complete, skipped, aborted, late, or still open when playback was stopped); below them, one lane shows what was rendered. A track with too many groups to draw singly (audio) is drawn as one column per second.
  - **Reading it:** the time shown can be 60, 10 or 2 seconds (at 2 seconds each audio group is its own mark), the display can be paused, and pointing at a group picks out both its received bar and its rendered span.
  - **How it is fed:** the viewer reports group and frame events to an observer it is given, and the publish board records the groups it sends through the same recorder; nothing is measured inside `@qumo/moq`.

### Changed

- **Bumped `github.com/qumo-dev/gomoqt` to v0.22.1.** No relay code change was needed. It brings:
  - **WebTransport clients see why the relay ended their session.** When the relay ends a session at its grant's `expires` or on a refused revalidate, `moqt.Cause` now gives the client Unauthorized with the reason `expired` or `refused` over WebTransport, as it already did over native QUIC. A client can reconnect with a fresh credential on `expired` and stop on `refused` (#423). The lease integration tests now check this on both transports.
  - **quic-go v0.63.0,** and webtransport-go `v0.13.0-okdaichi.2` (synced with upstream v0.13.0). Dependabot's quic-go bump (#420) is included.
- **The playground's viewer is a UI-free module, and it no longer waits on a stalled group (`playground/src/player`).**
  - **Structure:** playback moved out of `SubscribeBoard` into a `Viewer` that knows nothing about SolidJS. It reads from a small Track / Group / Frame interface it defines itself; `@qumo/moq` is adapted to that interface in one file.
  - **Groups are read as they arrive,** instead of one at a time. Frames are still delivered in group order, but a missing or stalled group is waited for only so long: once a later group has had a frame ready for the playback delay (100 ms at least) and its media is further ahead than that, the awaited group is cancelled (`ExpiredGroup`) and playback moves on. A group that arrives after playback has passed it is cancelled at once. The scheme follows the moq-dev reference player, with the wait also measured on the clock, so groups that merely arrive together are not mistaken for late ones. Two further rules keep playback at the live edge: a group whose frames are all played and which the next group continues without a gap is finished without waiting for its stream to end, and a group that is already too late when its turn comes is skipped whole if a newer one is ready (so a stall is not followed by its backlog).
  - **A group aborted part-way no longer ends the track:** its frames so far are played and the next group follows.
  - **Audio and video are played on a clock.** Both trail the live edge by the same delay: room for one retransmit (1.25 × the connection's round-trip time), and never less than 100 ms. Video frames are held until they are due instead of being drawn as soon as they decode, so video no longer runs ahead of audio. The delay is also how long a group is waited for. It is sized once per Start, and the audio output is opened per Start to match. Each track keeps its own clock, since their timestamps need not start from the same origin.
  - **Fixed:** a failed start left the other tracks' read loops running, so pressing Start again could feed the decoder twice; the catalog subscription was never closed; stopping before the first catalog arrived left the video and audio subscriptions open.
  - **Tests:** the group reader, the playback clock and the frame pacer have unit tests (`deno task test`), which `deno task build` now runs first.

### Fixed

- **RTMP and RTSP ingest no longer sends an old group in place of a new one.** A subscriber that asked for the newest group just as it was being opened could be handed the group eight sequences back, and never got the new one. With one audio frame per group this lost about one frame a minute, heard as a click.

### Security

- **A relay URL's credential is never logged (#432).** `loadgen` logs only the relay's `host:port`, `smoketest` a URL without its query, and the HLS egress doesn't log `RELAY_URL`. A URL that doesn't parse is reported without quoting it.

### Changed (breaking)

- **`qumo_relay_sessions_expired_total` is replaced by `qumo_relay_sessions_ended_total{reason}`,** with `reason` `expired`, `refused` or `invalid`. The old metric shipped only in v0.9.261004; use `reason="expired"` for the same count. `qumo_relay_auth_requests_total{event}` now also counts `revalidate`.

## [v0.9.261004] - 2026-10-04

> **Breaking for operators.** Session auth now goes through an external auth server (`QUMO_AUTH_URL`; unset means auth off, with a warning), and introspection is removed. Relay peers are identified by a client certificate verified against `CA_FILE`; `PEER_CIDRS`, `MTLS_REQUIRED` and `UPSTREAM_ADDR` are removed (use `PEERS`). Sessions end at their grant's `expires`. **Not yet enforced:** revalidation (#419), and which paths a session can discover through announce interest and TRACK_INFO (#418).

### Changed (breaking)

- **`relay.Server.Authorize` is exported (`internal/relay`, #444),** so code elsewhere in this module that builds a relay, such as the black-box tests, can supply its admission. A nil grant with a nil error admits unchecked, an `auth.RefusedError` refuses with its status, and any other error refuses as unavailable. A nil `Authorize` still refuses every client session. The package stays `internal/`, so code outside qumo can't import it yet. The admission tests moved to `internal/integration` and test the relay through this exported API; CONTRIBUTING.md now says where integration tests go.
- **Auth is optional: with `QUMO_AUTH_URL` the relay asks its auth server, without it auth is off; `QUMO_AUTH_PUBLIC` is removed (`internal/relay`, `internal/auth`, #441).** qumo is a data plane that runs on its own: auth is off unless `QUMO_AUTH_URL` is set, like Caddy's or nginx's. What a session may do is otherwise decided only by the auth server, whose policy differs by app, so qumo ships none. A static public grant in the relay was policy in the data plane.
  - **Auth off:** every session is admitted unchecked, like a trusted peer's, and the relay logs a warning at startup. No auth server is asked, and the admission is counted as `unchecked`.
  - **In code:** the relay takes its check as a function field, `Authorize`. The relay command sets it to the auth client's `Connect`, or to `admitUnchecked` when auth is off; tests pass a plain function. Removed: the `admitter` interface, `publicGrant`, and `auth.Config.Public`.
  - **Fail-closed:** a `Server` built without `Authorize` refuses every client session (WebTransport gets 503) and logs an error. Turning auth off is a setting; forgetting `authorize` is a bug.
  - **Only an upgrade asks:** a request to the WebTransport endpoint that isn't an extended CONNECT goes straight to gomoqt's fallback (400), without asking the auth server.
  - **Local tools, compose and the Nomad demo** run with auth off; they no longer set `QUMO_AUTH_PUBLIC`.
  - **Fixed:** `qumo playground` had no auth setting, so its relay stopped at startup. Auth off now applies.
  - **The auth-server client moves to its own package, `internal/auth`:** `LoadConfig`, the client, the grant (decoded through `json.Unmarshaler`) and its subtree patterns. `internal/relay/admit.go` keeps what is relay-specific: building the request from a WebTransport upgrade or a native-QUIC session, the session's admission, and the metrics.
- **Subscriptions are checked against the session's grant (`internal/relay`, #418).** A SUBSCRIBE is served only if the grant's `subscribe` patterns cover its path; otherwise it's refused with `NotFound`, the same answer as a path that doesn't exist. The session stays up.
  - **Every session's context carries its admission:** decided at the upgrade for WebTransport, and pending from `ConnContext` until `relayPeer` decides it for native QUIC. A subscription that arrives before admission finishes waits for it. Announcements are checked against the same admission, so `serveSession` no longer takes a grant.
  - **Unchanged:** trusted peers and dialed peers are never checked. FETCH stays rejected: the relay registers no fetch handler.
  - **Still open on #418:** path names and metadata are still discoverable, never media. Announce interest lists every path under the requested prefix, and a TRACK request returns a track's publisher properties (TRACK_INFO) for any path. gomoqt answers both inside the shared `TrackMux`, and `TrackInfoProvider.TrackInfo` has no context to tell which session is asking.
  - **New metric:** `qumo_relay_subscribe_authorizations_total{result}` (`admitted`, `not_covered`).
- **Relays ask an auth server when a session connects; introspection is removed (`internal/relay`).** First step of the relay side of the auth redesign (#417, epic #426).
  - **Setting:** `QUMO_AUTH_URL`, the auth server. (This change first also required one of `QUMO_AUTH_URL` or `QUMO_AUTH_PUBLIC`; auth has since become optional and `QUMO_AUTH_PUBLIC` was removed before release, see #441 above.)
  - **The contract** is a subset of `moq-auth`. The relay POSTs a `connect` request with the session's path and raw `query`, and enforces the grant's `publish` patterns on announcements. The relay never parses the credential, which clients put in the connect URL (`?jwt=`).
  - **Refusal:** a WebTransport client gets 401, 403 or 503 before the upgrade; a native-QUIC session is closed with `0x2`.
  - **Peers:** trusted peers are never asked. Peers this relay dials (`PEERS`) are now trusted too.
  - **Removed:**
    - introspection (`POST /v1/credentials/introspect`) and the `auth` track;
    - local verification against the control plane's JWKS (`internal/credential`);
    - `QUMO_CREDENTIAL_URL`, `QUMO_RELAY_TOKEN`, `QUMO_RELAY_AUDIENCE`, `QUMO_CREDENTIAL_ISSUER`;
    - usage reporting to `POST /v1/usage/events`. Session bytes return through the auth contract in #424.
  - **New metrics:** `qumo_relay_auth_requests_total{event,result}`, `qumo_relay_announcements_refused_total`.
  - **Requires** gomoqt v0.21.0: the WebTransport upgrade request's context reaches the session, and a native-QUIC client's `?jwt=` reaches the relay in the SETUP path (`Session.RequestURI`).
  - **Docs:** the remaining "credential auth" wording (README, the `qumo relay` CLI page) now says session auth, and an egress comment no longer mentions the removed metering.
- **Peers are identified by certificate; peer settings are reduced to `CA_FILE` and `PEERS` (`internal/relay`).**
  - **Trusted peer:** a session whose client certificate is verified against `CA_FILE`. A client certificate is optional for everyone else, so browsers connect without one. Without `CA_FILE`, no session is a peer.
  - **Dialing:** relays in `PEERS` are verified against the system roots plus `CA_FILE` (before, `CA_FILE` replaced the system roots). This relay presents its `CERT_FILE` as its client certificate.
  - **`PEERS`** resolves each host to all its addresses and dials every one, which `UPSTREAM_ADDR` used to do. Entries are plain `host:port`.
  - **Removed:** `PEER_CIDRS` (network-based trust), `MTLS_REQUIRED` (a client certificate is now always optional, verified when given), `UPSTREAM_ADDR` (use `PEERS`).
  - **Migration:** a relay that relied on `PEER_CIDRS` needs a private CA, with a client certificate for each relay, before upgrading. Native-QUIC tools (ingest, HLS egress) don't: they connect with `?jwt=` and are admitted by the auth server.

### Added

- **Sessions end at their grant's `expires` (#423).** The auth server sets `expires` to the credential's `exp`. The relay closes the session then, publishers and subscribers alike, with `0x2` (Unauthorized) and reason `expired`. The client reconnects with a fresh credential.
  - **The deadline** is taken on the monotonic clock when the grant is accepted, so a wall-clock jump doesn't move it. Expiry is exact: any leeway is the auth server's.
  - **One timer per session,** stopped when the session ends first.
  - **No deadline** for a grant without `expires`, a trusted peer, or a relay with auth off.
  - New metric: `qumo_relay_sessions_expired_total`.
  - Before this, a session outlived its credential indefinitely, and expiry is how an app cuts a user off (ADR 0035).

### Changed

- **Bumped `github.com/qumo-dev/gomoqt` to v0.22.0.** No code change was needed. It brings:
  - **Tracks the relay forwards end promptly.** The relay forwards groups with `OpenGroupAt`, which made gomoqt's SUBSCRIBE_END name a group that was never sent. Every downstream subscriber then waited out a close grace period before `io.EOF`.
  - **A native-QUIC client that fails SETUP is disconnected.** Before, the connection stayed open with no error code, and before any auth ran.
  - **Control messages are capped at 64 KiB.** A peer could make the relay reserve up to 50 MiB per stream by declaring a long message and never sending it.
  - **GOAWAY is a hint.** After a peer relay's GOAWAY, its session keeps serving subscriptions until it closes, which matches how `handlePeerGoaway` already treats it. Closing the session afterwards now really closes the connection.
  - **Leak fixes:** a failed subscription no longer leaks its registration or queued group streams, and an announce stream the peer ends releases its reader and ends the broadcasts it announced.

## [v0.8.260929] - 2026-09-29

### Added

- **Local credential verification for self-hosted relays (`internal/credential`, `internal/relay`).**
  - **What it does:** with `QUMO_RELAY_AUDIENCE` set, the relay verifies publisher credentials itself against the control plane's JWKS instead of calling `POST /v1/credentials/introspect`. It checks the EdDSA signature by `kid`, `exp`/`nbf`/`iat` with 60 s leeway, `iss`, `aud`, and that `path_auth` covers the announced path, matching the control plane's rule.
  - **Key set:** fetched at start (nothing is admitted until it succeeds), refreshed every 5 minutes, and refetched on an unknown `kid` at most every 30 s. It stays fail-static for up to 6 hours if refreshes fail.
  - **Intended use:** a relay a customer runs for a dev project (`QUMO_RELAY_AUDIENCE=qumo-relay-dev`). No relay token needed, no revocation feed (a revoked credential stops working at its expiry), no usage reporting.
  - **Managed relays are unchanged.** `qumo-relay` is refused until they consume the revocation feed (#419).

- **Ramped session admission for capacity probes (`internal/loadgen`, `tools/capacity`).**
  `qumo loadgen subscribe` gains `--ramp R` (sessions/second; the `tools/capacity`
  driver passes it through as `--ramp`). The burst remains the default (ramp 0) and
  `dialWithRetry`'s reactive backoff is unchanged — the new pacing spaces the *first*
  dials, which is what a steady-state holding measurement needs: a synchronized burst
  caps on simultaneous QUIC handshakes (relay 250–320 % CPU + 32–45 % softirq; ledger
  C23/C24) long before the relay's holding ceiling, so burst probes at S≥6000 measure
  admission capacity, not holding capacity. Ramped runs extend the settle deadline by
  the pacing window, record `ramp_per_sec` and `launched` in the JSONL row, and
  compute the verdict over launched sessions so a mid-ramp abort (Ctrl-C) stays
  well-defined.

## [v0.8.260927] - 2026-09-27

### Changed

- **Native-QUIC sessions are trusted as relay peers only when identified.**
  With credential auth on (`QUMO_CREDENTIAL_URL`), a native-QUIC session now
  bypasses per-announcement authentication only if it presents a client
  certificate verified against `CA_FILE` (mTLS) or connects from a network in
  the new `PEER_CIDRS` setting (e.g. a mesh overlay such as `100.64.0.0/10`).
  Any other native-QUIC session authenticates its announcements exactly like a
  WebTransport client. **Operators running with credential auth must set
  `PEER_CIDRS` or configure mTLS for their relay-to-relay and ingress links**;
  the relay logs a warning at startup when neither is set. Relays without
  credential auth are unaffected.

## [v0.7.260927] - 2026-09-27

### Added

- **Credential introspection includes the announced broadcast path.**
  `POST /v1/credentials/introspect` now carries `broadcast_path` (the path from
  the ANNOUNCE) alongside `token`, so a credential server can issue credentials
  scoped to a path and check them against the actual announcement. The relay
  still makes no authorization decision itself. Results are cached per
  (token, path). Credential servers that ignore the field are unaffected.
- **Interactive console UI for the installer scripts (`install.ps1`, `install.sh`).**
  A braille spinner animates the release download, milestones report as green
  check marks, and secondary detail (archive name, install path) is dimmed.
  Styling is suppressed when output is not an interactive terminal, and the
  spinner falls back to a plain line where sub-second `sleep` is unavailable.
  `install.ps1` is kept pure ASCII so Windows PowerShell 5.1 parses it
  identically under every system codepage.

- **Installer sync gate (`scripts/check-installer-sync.sh`, CI job "Installer
  scripts").** `install.sh` and `install.ps1` are each published twice — from
  the repo root via `raw.githubusercontent.com` and from `docs/site/static/`
  via GitHub Pages — and both URLs are documented, but nothing generated one
  copy from the other. An unmirrored edit shipped a different installer to
  whichever half of the docs pointed at the stale URL, and the diff looked
  complete either way. CI now fails on drift, and separately on any non-ASCII
  byte in `install.ps1` (the parse hazard behind #400).

### Changed

- **Zero-listener ingest notify fast path (`internal/ingest`).**
  `broadcastNotify.notify()` used to close and recreate its notification
  channel on every pushed frame even when no egress goroutine was attached —
  a mutex round-trip plus two allocations (~128 B) per frame, 37.9 % of
  alloc-objects in the zero-subscriber publisher profile, spent waking
  nobody. The sequence number is now authoritative (a plain atomic) and the
  swap is gated on a listener count registered per `serve()` lifetime: with
  no listeners a notify is a single atomic add with zero allocations
  (nolisten 72 ns/2 allocs → 7–11 ns/0); with listeners the wake path is
  unchanged. Restores the pre-#397 zero-subscriber allocation floor.

### Fixed

- **`TestMetrics_Initialization` failed under `go test -count=N` (N ≥ 2)
  (`internal/relay`).** The test asserted absolute values on package-global
  Prometheus counters, which accumulate across repetitions (every counter read
  exactly 2× on the second run). Counter assertions are now deltas around a
  single increment; gauge and histogram assertions were already
  repetition-stable and are unchanged.

## [v0.7.260922] - 2026-09-22

### Changed

- **O(1) ingest fan-out notification (`internal/ingest`).** Replaces
  `trackBuffer`'s per-subscriber notification channels with the relay's
  `broadcastNotify` (atomic sequence number + close-and-recreate channel, the
  mechanism proven in production by #332). Every pushed video/audio frame used
  to walk one channel per subscriber under an RWMutex — O(N) per frame on the
  ingest critical path, ~55 µs per frame at 1000 subscribers — and every
  subscriber cost a channel plus two registry operations on
  subscribe/unsubscribe. A push now advances a sequence number and closes one
  channel (~70–130 ns, 2 small allocations, flat in N); egress compares
  sequences and parks on the current channel, with the timer fallback and
  cancellation semantics unchanged. Measured (benchstat, n=6, local):
  notification at 1000 kept-up subscribers 54.9 µs → ~0.8 µs; at 100,
  3.4 µs → ~0.5 µs; at N ≤ 10 the writer path is ~30–130 ns/frame slower and
  every frame carries 128 B more garbage — the same trade the relay accepted
  in #332 (see the optimization ledger, C17). Correctness: no lost wakeup —
  the seq guard covers a notify that fires between the head check and the
  park; the intra-group trickle wait keeps its notifyTimeout fallback.

### Added
- **Zero-prerequisite one-line installation scripts (`install.ps1`, `install.sh`).**
  Added automated one-line installation scripts for Windows (PowerShell) and
  Linux / macOS (POSIX shell) that auto-detect OS and architecture, verify SHA-256
  checksums against GitHub Releases, install the binary to user space (`~/.qumo/bin`),
  and configure `PATH`. Also hosted on the documentation site (`/install.ps1`, `/install.sh`).

- **Per-track relay subscription and upstream request metrics (`internal/relay`).**
  Adds Prometheus gauges for active subscriptions, distributor reuse and
  upstream request counters, errors, and duration, labeled by observed
  broadcast path and track name.

- **`/routes` relay route snapshot endpoint (`internal/relay`).** Exposes
  active broadcast paths with their selected upstream source, hop count,
  RTT estimate, bitrate, and route update time for incident investigation.
  This complements Prometheus `/metrics`, which remains the source for
  time-series dashboards and alerts.

- **`UPSTREAM_ADDR` configuration for hierarchical and edge relays (`internal/relay`).**
  Edge relays connect upstream to regional hub relays (or hierarchical relays
  connect to upstream relays) specified via `UPSTREAM_ADDR`. When a DNS hostname
  (such as Consul DNS `role-hub.qumo-relay.service.consul:4433`) is supplied,
  all resolved A and AAAA records are connected to so that the edge acts as a
  multi-homed L7 load balancer with automatic connection maintenance and backoff.

### Removed
- **Retired legacy `LocalResolver` and `RemoteResolver` (`internal/relay`).**
  Replaced dynamic control-plane peer discovery microservices and Nomad API
  polling with direct DNS and static peer configuration (`UPSTREAM_ADDR` / `PEERS`),
  and cleaned up remaining historical references across docs and Nomad manifests.

### Changed
- **Bumped `github.com/qumo-dev/gomoqt` to v0.18.0 (#392).**
  - Implemented `moqt.TrackInfoProvider` across `internal/ingest` and `internal/relay`
    to answer `TRACK_INFO` control requests with immutable publisher properties
    (delivery priority, group ordering preference, max latency, and timescale)
    as specified in `moq-lite` draft-05.
  - Aligned MOQ Streaming Format catalog generation and parsing with
    `draft-ietf-moq-msf-01` §5.1.7 and §5.2.13 (`InitRef` referencing
    `InitDataList` entries rather than inline `InitData` strings).
  - Updated `moqt.Dialer.DialQUIC` callers across the relay server and integration
    tests to supply the new `path` parameter.

- **Bumped `github.com/qumo-dev/gomoqt` to v0.20.0, and the playground's
  `@qumo/moq` from 0.17 to 0.20.**
  - Moved relay peer dialing (`maintainPeer`) and the integration tests off
    `moqt.Dialer.DialQUIC`, removed in gomoqt v0.20.0, onto `moqt.Dialer.Dial` with a `moqt://` URL built by a new
    `peerURL` helper. No behavior change: `Dial` with no path dials the same
    `/` endpoint that `DialQUIC(ctx, addr, "", mux)` did, and resolved peers
    reach it via `net.JoinHostPort`, so IPv6 literals are already bracketed as
    URL syntax requires. The IPv6 dual-stack and upstream peer-discovery
    integration tests exercise both.
  - Bumped the web playground's `@qumo/moq` pin from `^0.17.0` to `^0.20.0`.
    With #392 alone the playground was cut off from the relay twice over: 0.17
    speaks moq-lite draft-04 while the relay now speaks draft-05, and its `msf`
    parser read only inline `initData`, which the msf-01 catalogs from ingest no
    longer carry, so the player got no AVC decoder configuration or AAC
    `AudioSpecificConfig`. `@qumo/moq` 0.20 speaks draft-05 and resolves each
    track's `initRef` back into `initData`, so the player code is unchanged. The
    publisher side is fixed the same way: `Broadcast` now writes the board's
    inline `initData` as an `initDataList` entry, which HLS egress
    (`internal/hls/feed.go`, `initFromTrack`) resolves. `playground/dist` is
    rebuilt with the CI-pinned deno 2.8.1 and verified reproducible.

- **Use standard library `uuid` package for broadcast session IDs (`internal/relay`).**
  Replaced hand-rolled UUID v4 generation (`crypto/rand` + `encoding/hex`) in
  `newUUIDv4` with Go 1.27's standard library `uuid.NewV4()`.

- **Hub role dials all remote hubs, not just the first (`internal/relay`).**
  Cross-cluster hub↔hub links previously dialed only the first resolved
  remote peer — a single point of failure for the region's inter-cluster
  connectivity, and resolver-order herding where every hub landed on the
  same remote peer. Hubs now dial the remote hubs the registry returns,
  minus self (the registry lists every hub, including the requester) and
  minus peers whose side owns the pair's outbound session per a
  deterministic node-ID tie-break — without it, both hubs of a pair dial
  each other in the same tick window and carry two parallel sessions.
  Multiple remote announcements of the same broadcast are resolved
  per-track by route election (`compareRoutes`). The remote resolver now
  sends `?hub=<RELAY_NAME>` as a proper query parameter (a base URL that
  already carried a query was previously mangled into the path), letting
  the registry skip the requester's row and bound the mesh degree, and
  peer-resolution failures are logged instead of silently skipping the
  tick. Prerequisite for multi-region topologies (see discussion #379);
  peer-lifecycle follow-ups tracked in #381.

### Fixed
- **RTSP pull sessions now send periodic keepalive requests (internal/rtsp,
  internal/ingest).** Prevents servers that enforce RTSP Session timeouts
  from dropping healthy but otherwise idle TCP-interleaved streams.
- **Docs site: corrected claims that contradicted the code** — verified every
  documented default, metric name, and flag against the source and a running
  binary. `RELAY_PPROF` was listed with a default of `0`, implying `0` disables
  it; the relay checks the variable for *emptiness*, so `RELAY_PPROF=0` in fact
  turns pprof **on** and exposes heap/goroutine dumps — the row now reads
  `(unset)` and carries a warning callout. `LOCAL_RESOLVER_ADDR` was credited
  with being "set automatically when running inside Nomad"; Nomad sets
  `NOMAD_ADDR`, which the relay falls back to, and which was undocumented.
  The GC guidance said `RELAY_GOGC=800..1600` on one page and `600..1600` on
  two others; the code says 600. The CLI reference's usage transcript omitted
  `qumo update` entirely, which now has its own page. Route election is
  described with the hysteresis margins added in #373 (bitrate 120%, RTT 5 ms
  absolute *or* 80% relative), and `qumo_relay_route_rejections_total` no
  longer claims rejected candidates "weren't better". Building from source no
  longer claims to require Deno: `playground/dist` is committed, so a plain
  `go build` embeds the real UI, and Deno/Mage are needed only for
  `mage webbuild`/`mage build`.

## [v0.6.260906] - 2026-09-06

### Fixed
- **`go install` now embeds the real playground web UI (#376)** — the built
  `playground/dist` is committed to the repository (Go module archives contain
  only git-tracked files, so a dist-less tag embedded the placeholder and
  white-screened `qumo playground` with a module MIME-type error). A new CI
  job ("Web UI dist freshness") rebuilds the UI and fails when the committed
  dist doesn't match, and the release workflow verifies the same before
  publishing a tag; `playground/README.md` documents the rebuild-and-commit
  workflow (`mage webbuild`), the pinned toolchain, and how to recover the
  CI-built bundles when a local rebuild disagrees. `go test ./...` also
  asserts the embedded tree really contains the bundles, so the regression
  is caught without a JS toolchain.
- **`qumo playground` placeholder-dist detection (`internal/playground`)** —
  as a safety net, the playground command detects a bundle-less dist at
  startup (no Vite output embedded), logs a warning naming the missing files
  and the rebuild command, and serves an explanatory error page in place of
  the broken UI. The relay and `/config` continue to work.

## [v0.6.260903] - 2026-09-03

### Added
- **`qumo update` self-update command (`internal/update`)** — downloads and
  replaces the running binary with the latest release from GitHub (`qumo-dev/qumo`),
  verifying SHA-256 checksums against GoReleaser's `checksums.txt`.
  - Supports `--check` for dry-run checks without modifying the binary.
  - Automatically identifies system architecture and OS (`tar.gz` for Linux/macOS,
    `.zip` for Windows).
  - Handles version comparison supporting SemVer and SemCalVer (`v[major].[minor].[YYMMDD]`).
  - Gracefully skips development builds (`version="dev"`).
  - Uses only Go standard library with zero new dependencies.

  **Usage Examples:**
  ```bash
  # Check if a new release is available (dry-run)
  $ qumo update --check
  qumo v1.0.260903 is available (current: v1.0.260902)

  # Check when already on the latest version
  $ qumo update --check
  qumo v1.0.260903 is already up to date

  # Apply update to the latest release
  $ qumo update
  qumo: updating v1.0.260902 → v1.0.260903 ...
  qumo: updated to v1.0.260903

  # Running in local development builds
  $ qumo update
  qumo: dev build — skipping update check
  ```

- **`qumo hls` HLS/DASH egress subcommand (`internal/hls`)** — feeds a MoQ track
  from a relay into qumo-ledger and serves the ledger's HLS playlist and DASH
  MPD over HTTP. Packaging happens at the subscriber (`internal/cmaf`): each MoQ
  group of LOC frames becomes one CMAF (fMP4) fragment in a microsecond
  timescale, with sample durations measured from the LOC timestamp gaps, so the
  segments are HLS-playable. Depends on qumo-ledger v0.1.0, consumed as a normal
  module.
- **`qumo loadgen` end-to-end latency reporting** — subscribers decode the
  publisher's UnixNano stamp (payload bytes 8–16) and record delivery latency
  in a lock-free histogram (0.1 ms buckets, 1 s ceiling); the histogram is
  reset after the settle phase so reported p50/p95/p99 and frame counts cover
  the steady-state hold only. Printed in the carry report and emitted to
  `results.jsonl` (`lat_p50_ms`/`lat_p95_ms`/`lat_p99_ms`/`frames_recv`).
  Single-host shared-clock semantics: absolute values include co-located
  loadgen scheduling; cross-topology comparisons under identical load are the
  intended use.
- **Stage-latency instrumentation (`-tags instrument`)** — per-stage relay
  pipeline latency histograms (ingress append, ring residence, group open,
  frame write) behind a build tag; zero-overhead no-op in the default build.
  Benchmarks read steady-state p50/p95/p99/max via `Server.StageLatency()` /
  `StageLatencyReset()` for latency attribution (benchmark-time diagnostic).
- **Go benchmark controller (`bench-multiproc/cmd/benchctl`)** — replaces the
  bash orchestration scripts with a native Go controller for multi-relay
  (hub + P edges) scaling sweeps: hardened multi-strategy port cleanup,
  AND-checked edge-liveness, subprocess subscriber mode, and a `/debug/stages`
  accept-pipeline counter endpoint (instrument build only; `{}` stub by
  default). Report in `docs/perf/MULTI-PROCESS-FANOUT-SCALING.md`.

### Changed
- **Dependency bump: gomoqt v0.17.0 → quic-go v0.61.0.** Bumps
  `github.com/qumo-dev/gomoqt` to v0.17.0, which pulls `quic-go` v0.61.0
  (was v0.60.0) and `okdaichi/webtransport-go` v0.12.0-okdaichi.2 transitively.
  quic-go 0.61 adds WebTransport-oriented stream APIs (`TryWriteAll`,
  `WriteWithLimit`, `SetReceiveFinalSizeCallback`) and a 27% transport-parameter
  parse speedup; the breaking changes (`StreamID.Type`/`InitiatedBy` removal,
  `http3.ParseCapsule` → `CapsuleParser`) are absorbed by the bumped gomoqt /
  webtransport-go, so qumo's own code needs no changes. Supersedes dependabot #355.

- **Reusable OpenGroupAt deadline** — egress delivery no longer constructs a
  `context.WithTimeout` per delivered group; a per-subscriber reusable
  timer/context bounds the open instead. 354.8→57.8 ns and 4→0 allocs per
  delivery (benchstat, n=10); −32% `deliverGroup` CPU at 1000-subscriber
  fan-out. Efficiency change only: measured e2e latency is unchanged.
  Backpressure semantics (30 ms bound, drop-group-and-continue) preserved.

- **`MTLS_REQUIRED` now defaults to `true`** — once `CA_FILE` enables mTLS, the
  relay now requires every connection to present a client cert signed by that
  CA by default (`tls.RequireAndVerifyClientCert`). Set `MTLS_REQUIRED=false`
  to opt back into the previous permissive default
  (`tls.VerifyClientCertIfGiven`) for relays that also serve direct
  browser/WebTransport traffic. Only affects deployments that already set
  `CA_FILE`; relays without it are unaffected.

- **Go toolchain bump: 1.26 → 1.27** (Go 1.27.0, released 2026-08-19). Updates
  the `go` directive in all modules (`go.mod`, `magefiles/go.mod`,
  `tools/paramexp/go.mod`, `docs/site/go.mod`), the builder image
  (`golang:1.27-alpine`), the Pages workflow Go pin, and the documented Go
  requirement (README, CONTRIBUTING, install docs, bench-multiproc
  instructions). CI resolves Go via `go-version-file: go.mod`, so it picks the
  bump up automatically. No source changes required. CI's golangci-lint is
  bumped v2.11.4 → v2.13.2 in step — v2.11.4 was built with go1.26 and refuses
  to load a module targeting go 1.27.0.

- **Docker runtime image now upgrades base packages (`docker/Dockerfile`).**
  The runtime stage runs `apk --no-cache upgrade` before installing its
  packages, so patched releases shipped after the floating `alpine:latest`
  digest was cut are pulled in. Currently that picks up OpenSSL 3.5.8-r0,
  fixing CVE-2026-14456 (QUIC server unbounded-memory DoS) that the Build
  Image workflow's Trivy scan flags on every PR built since the CVE was
  published.

- **`go fix` modernization pass (Go 1.27 toolchain).** Runs `go fix ./...`
  across the root module and `tools/paramexp` (37 files; `magefiles` matched
  no packages). Mechanical rewrites only: `strings.Cut`/`CutPrefix` replace
  index arithmetic, `strings.SplitSeq`/`bytes.SplitSeq` replace
  range-over-split slice allocations, 3-clause counted loops become
  `for range n`, clamping `if` statements become `min`/`max`, map-copy loops
  become `maps.Copy`, pointer-helper functions become `new(expr)`, and
  `sync.WaitGroup` Add/Done pairs become `WaitGroup.Go`. Two `omitzero`
  suggestions in `tools/paramexp` were skipped by the tool as behavior
  changes and are intentionally not applied.

- **Relay route selection with hysteresis (`internal/relay`).** Replaces
  `isBetterRoute(…) (bool, reason)` with `compareRoutes(…) routeDecision`,
  adding bitrate (≥20%) and RTT (5 ms absolute OR 20% relative) hysteresis
  thresholds so edges don't all converge on the same hub from transient
  metric noise. Adds structured logging (`slog.Info`/`slog.Debug`) at each
  route decision and splits `metricRouteReplacements` into a CounterVec with
  a `reason` label for observability.

- **Dependency bump: golang.org/x/crypto v0.54.0 → v0.55.0** (indirect, via
  quic-go/gomoqt; pulls x/text v0.41.0). Fixes CVE-2026-56854 (CRITICAL,
  x/crypto/ssh) that the Build Image workflow's Trivy scan flags on every PR.

- **gofmt cleanup.** Reformats 16 files that had drifted from gofmt:
  stale struct-field/const-block alignment from edits committed without
  running gofmt, mis-sorted imports in two integration tests, and one
  compressed multi-field struct literal. Formatting only; no semantic
  changes.

### Removed
- **`ADVERTISE_ADDR`** — dropped from `internal/relay/cmd.go`, `Config`, and
  `qumo playground`. It was set into `Config.AdvertiseAddr` and logged, but
  never actually consumed by peer resolution or announcement handling; the
  wildcard-bind guard that required it is gone too. Also removed from
  `relay-config.example.env`, the Docker Compose files, and the Nomad job
  spec.
- **`INSECURE`** — removed from all Docker Compose files, the Nomad job spec,
  and `relay-config.example.env`. No Go code ever read this variable; it was
  leftover from an unimplemented "generate ephemeral self-signed cert" idea.
  Setting it had no effect — the relay still required `CERT_FILE`/`KEY_FILE`
  to exist. The compose files now carry a comment pointing to `mkcert` for
  generating the cert they mount.

### Fixed
- **Nomad/Compose demo topologies never actually applied `--role`.**
  `docker-compose.static.yml` and `docker/nomad/qumo-cluster.nomad.hcl` set a
  `ROLE` *environment variable*, but the relay's topology role is a CLI flag
  (`qumo relay --role hub|edge`) with no env equivalent — so every node in
  both demos was silently running as a flat/standalone relay. This mattered
  most for the Nomad sim, whose entire purpose is exercising the
  role-gated `LocalResolver` peer-discovery branch (edges connect to all
  local hubs; hubs take no local action) — that branch never engaged. Fixed
  by passing `--role` on the container command/args in both files.

- **`qumo rtsp` panicked on a malformed broadcast path.** `moqt.NewAnnouncement`
  panics if the path doesn't start with `/`, and `ingest.NewSession` called it
  without validating first. Now `NewSession` rejects an invalid path with a
  clean error before reaching gomoqt — protecting all three ingest entry points
  (`rtmp`, `rtsp`, `rtsp-push`).

- **`internal/playground` restricted pull-server CORS origins.** The on-demand
  RTSP pull ingest server (`:4543`) allowed any WebTransport origin (`"*"`),
  letting an arbitrary malicious page initiate a Cross-Site WebTransport
  session to the user's localhost ingest. Replaced with `cors.SameHost`,
  which permits the browser handshake despite the UI/pull-server port
  mismatch (port-agnostic hostname comparison) while rejecting cross-origin
  pages. Access via a distinct hostname string (e.g. `127.0.0.1` vs the
  `localhost` default of `VITE_RELAY_URL`) is rejected, matching the main
  relay's existing policy.

## [v0.5.0] - 2026-07-24

### Release notes — relay performance cycle

A focused relay-side optimization + capacity-characterization cycle. After it,
the relay's own hot-path code is **<1% of CPU and <2.5% of allocations** under
load — the remaining costs live in quic-go (egress/handshake) and the Go
runtime. Full evidence in `docs/perf/`.

Landed optimizations (measured):
- **O(1) broadcast notification (#332)** — atomic-seqnum + close-and-recreate
  fan-out, ~82–86 ns flat across 1–1000 subscribers (was O(N), ~32 µs at 1000).
  Also closes a missed-wakeup window in the trickle wait.
- **Fixed worker pool for group fills (#338)** — per-group goroutine+closure
  spawn replaced by a long-lived worker pool: 11→9 allocs/op (−18%), ~15% faster
  on `BenchmarkProcessGroup`, no regression on ring fill.
- **Batched egress counter (#333)** — one Prometheus `Add` per group (was per
  frame); flushes on mid-group error so written bytes stay counted
  (metering-accurate). Marginal throughput impact (the counter was never the
  bottleneck) — primarily correctness/alloc hygiene.
- **Configurable UDP receive buffer `RELAY_UDP_RCVBUF` (#329)** — burst-safe
  `SO_RCVBUF` (default 256 KB); kernel-capped by `net.core.rmem_max` on Linux.
- **Opt-in pprof endpoint `RELAY_PPROF` (#339)** — `/debug/pprof/*` for profiling
  the relay under out-of-process load.

Characterized capacity envelope (WSL2 single-host; **shape-valid, ±noise — not a
bare-metal claim**):
- Sustainable HOLD **~13K** concurrent subscriber sessions; establishment peak
  **~15K**.
- Throughput **≥200K objects/sec** at 2000 subscribers (no cliff reached at that
  fan-out).
- Per session: **~7 goroutines**, **~468 KB RSS** (steady-state, GOGC=800).
- At the ceiling the relay is **not the bottleneck**: ~30% CPU/core, GC p99
  ≤6.7 ms, 11 FDs. The ~13K attrition mechanism is a **leading hypothesis
  (recv-buffer), unconfirmed** — see #343.

Tuning options (all opt-in; defaults unchanged):
- **`RELAY_GOGC`** — raises the HOLD ceiling on bare metal (~13–15K → ~18–20K at
  GOGC 600–1600); on single-host WSL the ceiling is establishment/recv-buffer-
  bound so GOGC has only ~±5% effect there.
- **`RELAY_UDP_RCVBUF`** — UDP `SO_RCVBUF`; raise `net.core.rmem_max` on Linux to
  let a large value take effect.
- **`RELAY_PPROF`** — off by default; enable on a trusted interface only.

Deferred validation (estimates/hypotheses, **not yet measured**):
- Bare-metal envelope (#341); distributed-load true ceiling (#342); recv-buffer
  hypothesis (#343); multi-publisher throughput scaling + exact PPS cliff (#344).
- The "~25K sessions" figure is a CPU extrapolation, **not measured**.

### Changed

- **`internal/relay` group fills use a fixed worker pool instead of a per-group goroutine.** `processGroup` previously did `wg.Go(func(){...})` per accepted group — spawning a goroutine and allocating two closures (the dispatch body + the per-frame fill callback) on every group, on the ingest hot path. It now dispatches a `fillJob` struct to a pool of `MaxGroupFillsInFlight` long-lived worker goroutines (created once per `trackDistributor`), and the per-frame callback is the `onFrame` method. Concurrency is bounded identically (one in-flight fill per worker via the existing `fillSem`), shutdown order is explicit (`close(fillJobs)` → `fillWg.Wait()` → `close(done)` so every reserved cache is filled before egress sees `done`), and the slot is acquired before `ring.reserve` so cancellation never leaks a reserved cache. Measured on `BenchmarkProcessGroup`: 11 → 9 allocs/op (−18%), 454 → 377 B/op (−17%), ~15% faster; no regression on `BenchmarkGroupRing_Fill`. Supersedes #336 (rebased onto the #332 atomic-seqnum notify API).

### Fixed

- **`internal/ingest` build: duplicate `benchStartServer` redeclaration.** Two parallel RTSP bench merges each added an identical `benchStartServer` helper (`rtsp_accept_bench_test.go` and `rtsp_loop_bench_test.go`), a same-package redeclaration that broke `go test ./...` / `golangci-lint` on `main` and blocked every open PR's CI. The duplicate is removed from `rtsp_accept_bench_test.go`; the single shared helper lives in `rtsp_loop_bench_test.go` (alongside `benchAnnounce`).

- **`internal/ingest` RTSP session accumulation per connection.** `handleConn` deferred `sess.Close()` inside its request loop, so every successful ANNOUNCE on a long-lived RTSP connection stacked another deferred close — sessions (and their goroutines/announcement state) accumulated until the TCP connection ended. The loop now closes any previous session before establishing a new one, with a single outer deferred close for final cleanup. `BenchmarkRTSPAnnounceLoop` is added as a regression guard.

### Changed

- **`tools/paramexp/report` SVG path building uses `strings.Builder`.** `sweepSVG` and `responseSurfaceSVG` built their `<path d="...">` strings with `d += fmt.Sprintf(...)` inside a loop — O(N²) memory copies. Replaced with `strings.Builder` + `fmt.Fprintf`, amortizing allocations. `BenchmarkSweepSVG`/`BenchmarkResponseSurfaceSVG` are added as regression guards.

- **`internal/ingest` RTSP accept loop avoids per-connection `defer`.** The per-connection goroutine in `ListenAndServe` called `connWg.Done()` via `defer`; replaced with an explicit call after `handleConn` returns, removing the defer overhead from the accept hot path. `BenchmarkRTSPConnCycle` is added as a regression/improvement guard.

- **`internal/rtsp` `selectQop` is allocation-free.** The RTSP Digest "qop" parser (run during connection-setup auth header construction) used `strings.Split`, allocating a `[]string` each call. Replaced with an `IndexByte` scan that allocates nothing — 1 → 0 allocs/op and ~75% faster on `BenchmarkSelectQop`.

### Added

- **Opt-in pprof endpoint (`RELAY_PPROF`, `internal/relay`).** Mounts `net/http/pprof` on the relay's HTTP mux (alongside `/metrics`), gated behind `RELAY_PPROF=1` (off by default). Exposes `/debug/pprof/{heap,profile,goroutine,trace,...}` so the relay can be profiled under out-of-process load (e.g. `qumo loadgen` driving it to the session ceiling) — the relay previously had no profiling surface, so the dominant cost could only be guessed. Off by default: pprof exposes runtime internals, so enable it only on a trusted/loopback interface.

- **Configurable UDP receive buffer (`RELAY_UDP_RCVBUF`, `internal/relay`).** The relay's single QUIC listener socket now sets `SO_RCVBUF` to a burst-safe size (default 256 KB) via a custom `transport.QUICListener` wrapper, instead of relying on the OS default (~8 KB on Windows, ~208 KB on Linux). At high fan-out with thousands of subscribers connecting in a burst, the default buffer can overflow and drop QUIC Initial packets, causing connection failures; the override absorbs the burst during handshake demux. `RELAY_UDP_RCVBUF=<bytes>` configures it (`0` disables the override, falling back to the OS default). On Linux the kernel silently caps the value at `net.core.rmem_max` (documented in `relay-config.example.env`). Compile-time interface assertions guard the quic-go/gomoqt wrapper against upstream signature changes.

- **`tools/capacity` starts a fresh relay per probe in `--start-relay` mode.** Each session-count probe now spawns its own relay + publisher and tears them down, so every measurement is independent. Previously the driver reused one long-lived relay across all probes, so once a probe pushed past the ceiling the relay carried residual goroutines/heap into the next probe — which corrupted the sub-ceiling bisect steps (a WSL run had the 17K/18K probes collapse to ~12K connected with a *negative* RSS delta). Reuse would turn the capacity benchmark into a recovery/soak test — a different workload that belongs in a higher-level harness, not a flag — so there is deliberately no `--reuse-relay` escape hatch. Remote mode (`--relay <host>`, no `--start-relay`) is unchanged: the external relay is a persistent service the driver doesn't own, so it keeps one publisher for the whole run.
- **`qumo loadgen` CLI simplified to primitives + a standalone `tools/capacity` driver.** The `qumo loadgen` CLI is now just two pure remote-client primitives — `publish` and `subscribe <N>` (N is a positional arg) — that dial the relay you point them at (`--relay` + `--ca`) and never spawn a relay or generate a cert. The `sweep` subcommand and its relay-lifecycle flags (`--start-relay`, `--relay-cores`, `--gogc`, cert generation) are **removed** from the CLI. Orchestration — sweeping an explicit `--sessions` list, or `--auto` climbing (geometric/`--step`) to find the capacity ceiling with optional `--bisect` boundary refinement — now lives in a separate Go driver, `tools/capacity`, which composes the primitives: it generates a cert, starts a local relay (`--start-relay`, optionally `taskset`-pinned via `--relay-cores`) and a publisher, then probes session counts by running `qumo loadgen subscribe <N>` and reading the verdict from `results.jsonl`. This keeps the shipped CLI small and moves the (unshipped) bench orchestration into a dev tool, driven the same way locally and in CI. The climb/bisect search is pure and unit-tested (`tools/capacity/ceiling.go`); the `capacity-sweep` CI job (`bench-relay.yml`) builds and runs the driver for a modest core-pinned sweep, feeding the dashboard.
- **Connection-establishment retry/backoff for outbound peer dials (`internal/relay`, #305).** The relay's outbound peer reconnection (`maintainPeer`) previously used a fixed 5-second retry interval with no backoff or jitter, so a burst of simultaneous peer disconnects could trigger a thundering-herd of synchronized re-dials and overwhelm the peer's handshake capacity. Replaced with a `dialBackoff` struct that applies exponential backoff (1s base, 30s cap), ±25% jitter, and unlimited retries — transforming burst arrivals into a gradual ramp. The backoff state resets after a successful connection, so transient disconnects re-dial promptly. New metric: `qumo_relay_dial_retries_total{peer}` — every retry is counted and labelled by peer address, and the log now includes a `retry_attempt` field for operator observability.

- **`qumo loadgen` — out-of-process capacity load generator (`internal/loadgen`).** Drives a real, separately-running relay instead of the in-process integration benchmark. `loadgen publish` feeds a trickle track; `loadgen subscribe` ramps N subscriber sessions and measures the hold. The reason it exists: `BenchmarkRelay_ConnectionCarry` runs the relay and all N clients in one process on shared cores, so client-side QUIC-handshake CPU — not the relay — caps establishment (measured ~6K connected on an 8-core VM, collapsing past that; a GOGC A/B confirmed GC is *not* the establishment bottleneck: cutting relay GC CPU 12%→2% bought ~0 extra connections). `loadgen` separates the load generator from the relay (point `--relay` at another host, or pin them to disjoint cores on one box) and reports the **relay's own** per-session cost by scraping its `/metrics` before/after the ramp (`go_goroutines`, `process_resident_memory_bytes`, `qumo_relay_sessions_active`) — so the number reflects the relay under test, not the load. `subscribe --results <dir>` appends a `capacity`-group JSONL record in the same schema the dashboard reads, so an out-of-process sweep lands in the same consolidated `index.html`. Client trusts the relay via `--ca <relay-cert.pem>` (no insecure mode). Wired as a top-level `qumo` subcommand alongside `doctor`/`playground`.\n- **`qumo loadgen sweep` — session-count sweep + CI job (`bench-relay.yml` `loadgen-sweep`).** Runs a publisher plus a subscribe measurement per session count. Two modes: `--start-relay` spawns a local relay subprocess (self-signed cert generated in-process — no `openssl`/`curl`/shell dependency) pinned via `--relay-cores`, a single-box stand-in for two hosts that isolates the relay's CPU from the load; or `--relay <host:port> --ca <cert>` drives an existing relay on another machine (true two-host). Each point appends a `capacity`-group record to `results.jsonl`, which the dashboard renders. The new `loadgen-sweep` CI job runs a modest core-pinned sweep on the (small, single-host) hosted runner to exercise the path and publish a capacity dashboard artifact; distributed 25K-scale validation is a manual/self-hosted two-host run. Implemented as a Go subcommand (reusing the loadgen dial/measure/scrape code) rather than a shell script, to avoid environment mismatch across dev/CI/OS.\n- **Consolidated benchmark dashboard (`results/index.html`) + capacity records in the JSONL.** `scripts/relay_bench_report.ts` now emits a single self-contained `index.html` alongside the CSVs/SVGs — the \"easy to see\" surface: open it (no server, no external requests) and get the **capacity headline** (concurrent-session ceiling, per-session KB, goros/session), the decision summary (per-hop latency slope, fan-out knee K, jitter, fairness), every plot inline, and — when a paramexp report dir is passed via `--paramexp <dir>` — the GP/ML findings (best config ± CI, η² parameter importance, knees, interactions, suggested-next). The capacity headline is fed by a `capacity`-group JSONL record (sessions/connected/receiving/per_session_kb/verdict); the producer is `qumo loadgen` (see the load-generator entry). The `bench-relay.yml` `full` job produces the dashboard automatically; the paramexp GP findings ship in the separate `paramexp` job artifact and fold in locally with `--paramexp`.

- **Opt-in GC tuning for high-fan-out capacity (`internal/gctune`, `RELAY_GOGC`).** A fan-out relay holds a large, *stable* live set — one QUIC connection per subscriber, whose ~9 goroutines' stacks dominate RSS (measured: Go heap in-use ~200MB while RSS ~1.4GB at ~14K sessions; the gap is off-heap goroutine stacks). Every GC cycle re-scans all those stacks, so at the default `GOGC=100` the GC-scan CPU grows with connection count and becomes the scaling ceiling (measured on bare-metal 8 cores: default holds ~13–15K sessions; the collapse is GC, not memory exhaustion — RSS/session is flat ~127KB). Because the live set is legitimate and stable, collecting it less often costs only some peak RSS headroom while cutting GC CPU — setting `RELAY_GOGC` to 600–1600 reached **~18–20K concurrent subscriber sessions** on a bare-metal 8-core host (`BenchmarkRelay_ConnectionCarry`, slow-ramp), roughly doubling the default ceiling. The GC-scan mechanism was re-confirmed after the #313 poller consolidation (WSL2, 8-core, valid for shape not absolute ceilings): at 8K held sessions `GOGC=800` cut GC cycles 31→7 and GC CPU ~14%→~3% vs `GOGC=100` (same ~110 MB stacks scanned per cycle), for a higher heap goal (1.3 GB → 2.2 GB) — the documented peak-RSS-for-GC-CPU trade. #313 also lowered per-session goroutines to ~14–16 (from ~18–20), which only moves the GC wall up, so the bare-metal ceiling figures are conservative. The policy is **opt-in**: with neither `GOGC` nor `RELAY_GOGC` set the relay leaves the runtime default (100) untouched — no silent global behavior change; `GOGC` (the runtime's own knob) always wins and is never stomped; a valid positive `RELAY_GOGC` raises the target, and an invalid one warns and no-ops. **`GOMEMLIMIT` is deliberately not used** — capping memory for this large-stable-live-set forces constant GC into a death-spiral (measured: GOMEMLIMIT configs collapsed to ~15–18K while `GOGC=high` held 20K). The policy lives in a small `internal/gctune` package (pure `Resolve` + side-effecting `Apply`, unit-tested for env precedence) so the relay's startup path and the new `doctor` command share one source of truth.\n- **`qumo doctor` command — read-only runtime-config explainer.** Prints the effective GC target, every input (`GOGC`, `RELAY_GOGC`, `GOMEMLIMIT`), which input won and why, any warnings, and workload guidance — without mutating anything. Structured so future checks (sockets, QUIC, kernel) can slot in. Documented in `relay-config.example.env`.\n\n### Changed\n\n- **`internal/relay` metric sampling consolidated into one server-wide goroutine.** The relay previously spawned three long-lived poller goroutines per entity — `pollConnStats` (per native-QUIC connection), `pollSessionStats` (per session), and `pollCacheDepth` (per track distributor) — each a `for { <-ctx.Done(); <-ticker.C }` loop that sat parked on a 10–30s ticker. At high fan-out that is ~2 goroutines per session plus one per track (~40K+ goroutines at 20K sessions), and the dominant cost is **GC stack-scan**: every parked goroutine's stack is scanned on each GC cycle, so the measured wall at ~10K→20K sessions was off-heap goroutine-stack memory (RSS ~1.4GB vs heap ~200MB), not the heap itself. These are now replaced by a single `statsSampler` that holds three `sync.Map` registries (conns/sessions/tracks) and sweeps them from **one** goroutine per tick; entities register on start and deregister on teardown (connection-context `AfterFunc`, `serveSession` defer, and `ingest` defer respectively). Deregistration removes the registry entry (so the entity stops being sampled) and **queues** the Prometheus `DeleteLabelValues` onto the sampler goroutine, which drains the queue right after each sweep — so a series is never deleted concurrently with the sampler's own `Set` write, which would otherwise resurrect and leak it. A stale queued deletion whose addr was re-registered before it ran (ephemeral-port reuse) is skipped, so the new owner keeps its series. Metric semantics are unchanged (same gauges/histogram, same per-`remote`/`track` labels, same immediate first sample on register), except the per-addr `session_rtt_seconds` histogram series is now dropped on session end — the old per-session poller deleted only the two gauges and leaked one histogram series per departed session. All sampler methods are nil-safe so a minimally-constructed `Server` and standalone `trackDistributor`s (tests) skip sampling without guards. Per-addr gauge/histogram **cardinality** is unchanged and remains a separate follow-up.\n\n- **`internal/relay` groupCache is now a lock-free append-only vector (was copy-on-write).** Each `groupCache` published its frames as an immutable `atomic.Pointer[[]*moqt.Frame]` snapshot, and every `append` rebuilt the whole snapshot (`make(len+1)` + `copy`) before CAS-publishing it — O(N²) pointer copies and one slice allocation per frame across an N-frame group, all of it garbage the collector then had to reclaim. It is replaced by a fixed per-cache backing array of per-frame atomics (`slots []atomic.Pointer[moqt.Frame]`, sized `MaxFramesPerGroup`, allocated once and **reused** across group generations via the ring's `gcPool`) plus an atomic `count`. `append` now reserves a unique slot with a CAS on `count` (O(1), **zero allocation**) and Stores its clone; `next` remains a single atomic load. Concurrency and safety are preserved: appends stay concurrency-safe (the CAS reserves a unique slot, so no frame is lost or overwritten — `TestGroupCache_ConcurrentAppend` passes under `-race`), reads stay lock-free and data-race-free (distinct slots are distinct memory locations; `count`/slots touched only through atomics), and the reserve→Store window reads back as a nil frame, which the egress loop already treats as \"not ready, wait for the next broadcast\". This removes the per-append allocation and the O(N²) copy from the ingest hot path. Note the ingest/append path runs at publisher frame-rate (per track), not per-subscriber, so this reduces GC churn under high-ingest more than under pure fan-out.\n\n- **`qumo loadgen subscribe` / `sweep` — ramp flag deprecated (#327).** The `--ramp` flag is removed from `loadgen subscribe` and `loadgen sweep`. All subscriber sessions now launch in burst mode, relying on the exponential backoff in `dialWithRetry` (`DialBackoff`, 1s base, 30s cap, ±25% jitter) to spread out QUIC handshake load instead of a ticker-based ramp. The settle timeout is fixed at 30s (was `rampSecs + 10s`); safety deadline simplified to `hold + 60s`. Measured ceiling on single-host Windows: ~15K sessions (was ~8K with ramp), with the bottleneck shifting from the loadgen process to OS UDP socket buffer limits. Doc/usage strings updated to remove ramp terminology.

- **deps: `github.com/qumo-dev/gomoqt` `v0.16.1` → `v0.16.2-0.20260718145816-7bc42f96aec4` (merged `main`).** Pulls two per-session goroutine reductions that land in the relay's fan-out path: the **lazy bitrate monitor** (gomoqt #342 — the `detectBitrateChanges` goroutine no longer starts eagerly per session; subscriber sessions that never open a probe stream spend zero goroutines on it, with `EstimatedBitrate` preserved via lazy `Stats()` sampling) and **caller-driven `SUBSCRIBE_UPDATE`** (gomoqt #345 — the per-subscription background update-reader goroutine is gone; `TrackWriter.Updated() <-chan struct{}` was replaced by the blocking `TrackWriter.ReadUpdate() (*SubscribeConfig, error)`). Drop-in for qumo: the relay never used `Updated()` (nor `TrackConfig()`), so no source changes were needed — the whole module builds, vets, and tests green on the new dependency. Together these remove ~2–3 goroutines per subscriber connection (~20 → ~17 measured at 500 sessions), reducing per-connection footprint; they do **not** move the memory-bound session ceiling (per-conn goroutine-stack memory dominates, quic-go). Pins a pseudo-version of `main` pending a tagged gomoqt release.\n\n### Removed\n\n- **In-process capacity benchmarks removed (`internal/relay/capacity_bench_test.go`: `BenchmarkRelay_ConnectionCarry`, `BenchmarkRelay_CapacityFrontier`).** Superseded by `qumo loadgen`. Running the relay and all N subscriber clients in one process meant client-side QUIC-handshake CPU capped the run (~3–6K on an 8-core host, collapsing past that), so these benchmarks measured the test harness rather than the relay and were meaningless at the session ceiling — a GOGC A/B confirmed GC was not the wall (relay GC CPU 12%→2% bought ~0 extra connections). The concurrent-session ceiling is now measured out-of-process by `qumo loadgen`, which pins/relocates the load away from the relay and reports the relay's own per-session cost via `/metrics`. The `benchResult` `capacity`-group fields that only these benchmarks populated are dropped with them; the capacity JSONL schema now lives solely in `internal/loadgen`.\n- **`internal/relay` egress poll-fallback timer (`NotifyTimeout`) removed entirely.** The subscriber egress wait-select (`egress`/`deliverGroup`) carried a 1ms poll fallback, so every parked subscriber goroutine fired a timer 1000×/sec regardless of media rate — ~5M spurious wakeups/sec at the ~5K-session ceiling, the dominant `selectgo`/timer cost in prior scheduler profiles (and it is relay code, not quic-go). Investigation showed the timer was **never the delivery mechanism** and — contrary to the \"safety net\" assumption — not load-bearing at all: the per-frame `broadcast()` notify (`groupRing.fill`) wakes egress for every real delivery (a new group advances the ring head synchronously in `reserve()` then broadcasts; the notify channel is cap-1 buffered and subscribed before the loop, closing the enter-select race; egress re-reads `head()` fresh each iteration, so coalesced signals never lose data). Proven by `TestRelayChain_NotifyOnlyDelivery` (integration): with the timer disabled, all 40 gap-spaced groups are delivered promptly (max inter-arrival ≈ the 50ms publisher gap, not clumped at a fallback), and `TestRelayChain_SlowSubscriber` confirms the fell-behind path is covered by the next group's broadcast. Removing the timer arm from both egress selects is behavior-preserving (same frames, same order) and eliminates the idle-wakeup cost outright rather than merely coarsening it — the select keeps its two cancellation arms (`d.done`, `twCtx.Done()`), so shutdown convergence (#286) is unaffected. Reduces CPU/scheduler pressure and tail latency under load. **Correction (measured after merge):** this also *raises* the single-node session hold ceiling — an end-to-end `BenchmarkRelay_ConnectionCarry` sweep (WSL2 8C, reproduced) shows the ceiling jump from **~4.5K to 10K+** sessions with the timer removed, and an isolation run (goroutine reductions with the timer *still present*) stayed at ~4.5K — so the ~5K wall was this 1ms poll saturating the scheduler (~5M timer wakeups/sec), **not** per-connection memory as an earlier note claimed (per-session RSS is unchanged at ~127KB across the sweep). The `NotifyTimeout` package var, its `RELAY_NOTIFY_TIMEOUT`/`RELAY_NOTIFY_TIMEOUT_MS` overrides, and the tests that pinned its value are all removed.\n\n### Fixed\n\n- **`internal/relay` correct TrackWriter/OpenGroup usage.** `deliverGroup` now passes a deadline-bearing context to `OpenGroupAt` (30ms `defaultGroupTimeout`). When the peer's `MAX_STREAMS` limit is reached, the call blocks up to the timeout (gomoqt's designed backpressure via `OpenUniStreamSync`), then drops the group (MoQ semi-reliable) instead of blocking the egress goroutine indefinitely. Previously, the unbounded block caused stream-object accumulation and a GC-driven degradation spiral. `cmd.go` reverts `MaxIncomingUniStreams`/`MaxIncomingStreams` from `1<<20` back to quic-go defaults (~100) — the `1<<20` value (from #292) removed the backpressure entirely, which was the wrong fix; the timeout context is the correct one.\n\n### Fixed\n\n- **`tools/paramexp` post-merge review fixes (retro-review of #297/#298).** Six correctness bugs found by adversarial review of the merged code (same class as #294's GP-math bugs — CI-green but subtle math the tests asserted too little to catch):\n  - **Discrete-space selection (#298-1,2,3):** `SuggestedNext` could return duplicate configs and recommend already-measured points; `BayesianScheduler` could prematurely EOF in small discrete spaces (random-search argmax kept decoding to occupied cells). Both now use `model.SelectByAcquisition`, which enumerates the full discrete candidate set (guaranteed to find novel points if any remain) or random-searches continuous spaces with decoded-vector dedup.\n  - **Sample variance (#297-4):** `aggregateMetrics` used population variance (÷N) for inferential outputs (CIs, indistinguishable test, stability CV) — anti-conservative at small N (the replicate regime N=2–5). Switched to sample variance (÷N−1) and replaced the hardcoded z=1.96 with a `TCritical(df)` t-table (t₀.₉₇₅,df for df=1..30, z beyond). The \"95% CI\" labels now actually hold at small N.\n  - **Flaky-vector dominance (#297-5):** a config with only 1/N successful replicates got `Variances=0` → the GP treated it as near-noise-free and bent the surface through it (the *opposite* of \"downweight high-variance\"). `FitGP` now borrows the median noise of well-replicated points for N=1 configs.\n  - **`Fit` measuredNoise leak (#297-6):** `Fit` didn't reset `measuredNoise` (a `FitReplicated`→`Fit` reuse would apply the previous run's per-point noise to the new fit). Now resets at the top, matching `FitReplicated`.\n\n### Added\n\n- **`tools/paramexp` richer relay metric: jitter.** The fan-out bench now reports `jitter_ms` (sample stdev of per-group latencies) alongside loss/p99/mbps/fairness. The paramexp `bench.sh` harness emits it so the GP can model jitter as part of the landscape — a brief-listed metric that was previously missing.\n\n- **Relay performance-landscape sweep in CI (`bench-relay.yml`).** A new `paramexp` job runs the Bayesian-optimization sweep of relay tuning knobs (`example/relay/params.yaml`: ring size / frame / notify-timeout × fan-out K) nightly and on-demand. Each vector runs the integration fan-out bench via `bench.sh`; the GP + analysis produce a report (knees, importance, interactions, stability, suggested-next) answering \"which settings serve stable high-performance large fan-out?\" Uploads `paramexp_relay.db` + the report as the `relay-paramexp` artifact. `px_samples`/`px_replicates` workflow inputs tune the scale; the `bench.sh` harness is hardened (tolerant of failed/flaky vectors — degrades to a worst-case record instead of aborting the sweep).\n\n- **`tools/paramexp` Stage 2 — uncertainty-driven adaptive sampling (Bayesian optimization).** A `BayesianScheduler` (`scheduler: bo`) replaces the neighbor-of-best hill-climb: round 0 is an LHS seed batch (broad coverage), then each round fits the GP posterior and picks the next point(s) by maximizing an **acquisition function** — Expected Improvement (`ei`, default), Upper-Confidence-Bound (`ucb`, exploration knob κ), or predictive variance (`variance`, pure-exploration surface mapping). Acquisition is maximized by random search over `[0,1]^D` with decode-level dedup (so the discrete/categorical collision case never re-measures a known vector). Supports a batch (`bo_batch`) via greedy exclusion. CLI `--scheduler bo --acquisition ucb`; flat yaml knobs `bo_rounds`/`bo_batch`/`bo_acquisition`/`bo_kappa`/`bo_xi`. The `Scheduler` interface is unchanged, so the CLI driver loop is untouched.\n- **`tools/paramexp` acquisition functions + shared GP fit.** `model.NewExpectedImprovement`/`NewUpperConfidenceBound`/`NewPredictiveVariance`, `model.MaximizeAcquisition` (random-search argmax with exclusion), and `model.AcquisitionFor` (named resolver). `model.FitGP(obs, objective, opts)` dedups the fit logic (heteroscedastic when replicates carry variance) and replaces the inlined `cmd.fitGP`. `model.LCG` is now exported (reproducible random search).\n- **`tools/paramexp` suggested-next measurements.** `analysis.SuggestedNext(gp, enc, acq, n)` returns the top-N unmeasured points the model most wants to sample — the brief's \"what should we measure next.\" The report renders a \"Suggested next measurements\" section (text + JSON) with each point's predicted mean/std and acquisition value.\n\n### Added\n\n- **`tools/paramexp` replication + variance (statistical rigor).** Each parameter vector can now be run N times (`replicates:` config / `--replicates`); variance becomes first-class. `storage.Observations` aggregates replicates in Go (per-metric means + population variance + N), so analysis runs on the de-noised means. The GP gains a heteroscedastic `FitReplicated(X, yMean, yVar)` that uses the measured per-point variance as observation noise (the global noise hyperparameter stays as a floor) — high-variance configs are downweighted automatically and `var/N` shrinks as N grows. New analysis: `StabilityReport` flags configs whose objective CV exceeds `UnstableCV` (0.15), and `IndistinguishableFromBest` returns the best config plus the set whose CI overlaps it (the \"can't tell apart from best\" group). Reports now show `mean ± 95% CI (n=N)`, an unstable-configs section, best-vs-peers, and a caption explaining that η² (variance-explained) and GP 1/ℓ² (local relevance) measure different things.\n- **`tools/paramexp` → relay integration.** The relay fan-out benchmark is now sweepable by paramexp: `RELAY_RING`/`RELAY_FRAME` knobs in `spinRelay` and a `RELAY_NOTIFY_TIMEOUT_MS` knob in `fanoutSweepRun` (integration tests), plus `example/relay/{params.yaml,bench.sh}` — a self-contained harness that runs `BenchmarkRelayChain_FanoutSweep` per vector and emits one JSON line of loss/p99/mbps/fairness. This is the brief's \"Relay Integration\" layer; the full sweep belongs on the nightly Linux bench job.\n\n### Fixed\n\n- **`tools/paramexp` variance NaN.** `aggregateMetrics` used the numerically unstable `E[x²]−E[x]²` form, which goes slightly negative for near-constant (deterministic-bench) data and yielded `sqrt(NaN)` CIs — which in turn broke `report.json` marshaling (silent empty file). Switched to the two-pass `Σ(x−mean)²` form (never negative) and surfaced the marshal error instead of swallowing it.\n\n### Changed\n\n- **`tools/paramexp` package layout simplified (11 → 7 packages).** Folded the small leaf and coupled-pair packages into their natural homes to reduce over-decomposition: `encoding` → `experiment` (`experiment.Encoder`/`NewEncoder`; the encoder is the numeric view of the domain types, and everyone already imported `experiment`, so this also removes an import edge); `provenance` → `storage` (`storage.Run`/`Capture`/`Abs`); `visualization` → `report` (SVG helpers are now unexported, since only `report` ever used them); `scheduler` → `sampler` (`sampler.Scheduler`/`SchedulerState`/`StaticScheduler`). Final layout: `experiment`, `storage`, `runner`, `sampler`, `model`, `analysis`, `report`, plus the thin `cmd/paramexp`. The distinct heavy concerns (GP math in `model`, statistics in `analysis`, SQL in `storage`, exec in `runner`) stay separate.\n\n### Fixed\n\n- **`tools/paramexp` `report` package was never committed (#294 regression):** the module `.gitignore` rule `report/` — intended for the generated report *output* directory — also matched the `report/` source *package*, so `report/report.go` was silently excluded from #294. `cmd/paramexp` imports it, so `go build ./...` in `tools/paramexp` failed on `main` (CI didn't catch it because the qumo root `go test ./...` does not traverse the separate `tools/paramexp` module). The output directory is renamed to `report_out/` (default `--output`, gitignored) so it no longer collides with the `report` package, which is now tracked.\n\n- **`tools/paramexp` GP surrogate math (post-merge review of #294):**\n  - **Signal variance σ_f² was optimized but never applied to the kernel** (`model`): `K`, `k*`, and `k(x,x)` were built from the unit-variance RBF correlation with no σ_f² factor, so θ[D] was a dead search axis, `Hyperparameters().SignalVar` reported a value that never influenced the fit, and predictive variance was implicitly locked to σ_f²=1. The kernel correlation is now scaled by σ_f² at every build site via a `cov` helper.\n  - **Log-marginal-likelihood complexity term had the wrong coefficient** (`model`): `-logdet` was used where the GP LML requires `-0.5·log|K|` (`chol.LogDet()` returns `log|K|`). The doubled model-complexity penalty biased the optimizer toward shorter length-scales (rougher, overfitting posteriors) on every fit. Now `-0.5·logdet`.\n  - **`DetectKnees` missed the common concave/diminishing-returns case** (`analysis`): the single-sign `xNorm - yNorm` criterion only fired when the normalized curve lay below the diagonal, so a concave-increasing sweep (the default `throughput_fps` objective) returned no knee. Now uses `|yNorm - xNorm|` with decreasing-curve mirroring, finding the elbow for both concave and convex sweeps (the diminishing-returns knee on `workers` is now detected, where it previously was not).\n  - **`DetectRegressions` attribution was non-deterministic** (`analysis`): two independent map range loops could pair a `Param` from one key with a `Value` from another, varying across runs. `Regression` now carries the full offending `Vector` (deterministic, no information loss).\n- **`tools/paramexp` flat telemetry no longer contaminates metrics** (`runner`): `toMetricSet` now excludes the recognized telemetry keys (`cpu_pct`/`gc_pause_ms`/`syscalls`/`retransmits`/`rss_mb`/`goroutines`) so a benchmark emitting the flat telemetry shape does not pollute `RankImportance`/GP-fit/`--objective`. The nested `\"telemetry\"` shape was already clean.\n- **`tools/paramexp` in-memory storage DSN no longer drops pragmas** (`storage`): `:memory:` previously stripped `foreign_keys=ON` and did not pin the connection pool, so modernc/sqlite could route a query to a different connection's empty private DB. Pragmas now apply to all DSNs and `:memory:` pins `SetMaxOpenConns(1)`.\n\n### Changed\n\n- **`tools/paramexp` rewritten as a scientific performance-landscape framework.** The flat `package main` MVP is restructured into importable library packages (`experiment`, `encoding`, `provenance`, `runner`, `storage`, `sampler`, `model`, `analysis`, `scheduler`, `visualization`, `report`) plus a thin `cmd/paramexp` CLI — generic for any black-box benchmarkable system. Key additions:\n  - **Gaussian-process surrogate (`model`):** anisotropic RBF kernel with ARD length-scales, fit by maximizing the log-marginal-likelihood (multistart random search + Nelder-Mead polish via `gonum/optimize`, with a median-heuristic fallback), Cholesky-based solve via `gonum/mat`, adaptive-jitter numerical-stability handling, and a per-metric `MultiOutput`. Predict returns mean **and** predictive std (uncertainty) — the framework's first surrogate model and the foundation for Bayesian optimization.\n  - **Numeric parameter encoding:** parameters are now typed (continuous / discrete-ordinal / categorical) and mapped to a normalized `[0,1]^D` space the sampler and GP operate in; the runner still receives original string values. Continuous `min`/`max` and a continuous `jitter` dimension are demonstrated in `example/params.yaml`.\n  - **GP-derived analysis + viz:** `analysis.GPSensitivity` ranks dimensions by `1/ℓ²` (shorter length-scale ⟹ more sensitive); the report draws per-parameter response surfaces (mean ± 2σ band) and a 2-D contour over the two most-sensitive parameters.\n  - **Full provenance + retry + telemetry:** SQLite schema gains `runs` (git revision via `debug.ReadBuildInfo`, machine info, redacted env, config hash), per-retry `attempts`, and a `telemetry` table for resource snapshots (cpu/gc/retransmits/rss/goroutines) the benchmark may emit (feeds later bottleneck attribution). The runner enforces a real context timeout and retries with backoff.\n  - **Bug fixes from the MVP:** `DetectKnees`/`RankImportance` no longer hardcode `throughput_fps` (they honor `--objective`); `DetectRegressions` is no longer dead code and populates param/value; local `min`/`max` shadows of Go 1.21+ builtins removed; `Observations(includeFailures)` makes failed runs analyzable.\n  - New dependency: `gonum.org/v1/gonum` (pure Go, no CGO — consistent with the `CGO_ENABLED=0` posture). Sobol sampling is deferred to a roadmap phase-2 item: a first direction-number recurrence was not a true `(0,m)`-net (it degenerated to covering half the space), so `sampler.Sobol` falls back to LHS rather than ship a subtly-broken generator.\n\n### Added\n\n- **Automated parameter exploration framework (`tools/paramexp`):** A generic, black-box parameter optimization tool for any benchmarkable system. Samples a discrete parameter space via Latin Hypercube Sampling + adaptive neighbor exploration, runs benchmarks (params as `PARAM_<NAME>` env vars, JSON stdout metrics), stores every experiment in SQLite, then analyzes: knee points (Kneedle), parameter importance (η²), pairwise interactions, regressions, and generates SVG plots + JSON/text reports. One dependency: `modernc.org/sqlite` (pure Go).\n\n### Fixed\n\n- **Subscriber egress teardown hang (`internal/relay`, #286):** `trackDistributor.egress` now routes every non-delivery loop path through a single wait/cancellation `select` (on `twCtx.Done()`/`d.done`). Its fell-behind skip and cache-miss paths previously iterated via bare `continue` without consulting those signals, so a subscriber that fell behind could blind-spin past cancellation and never return when the subscriber disconnected or the relay shut down. That pinned gomoqt's stream-handler `WaitGroup`, so `Session.CloseWithError`'s `wg.Wait()` hung, the connection was never removed from the connManager, and `Server.Shutdown`/`Close` hung on `<-connManager.Done()` — the multi-subscriber teardown hang and churn-time goroutine leak. The cancellation signal already reached qumo (gomoqt's per-conn `goAway` force-closes the connection on ctx expiry, cancelling the subscribe-stream context `twCtx` derives from); qumo only needed to converge on the one select it already had. The per-group delivery body is extracted into `deliverGroup`. No gomoqt change required.\n\n### Changed\n\n- **Session-end handler cleanup moved to `newRelayHandler` (`internal/relay`):** the `context.AfterFunc(sess.Context(), cancel)` registration moved out of `installRoute` (where it needed a nil-session-context guard for test fixtures) into `newRelayHandler`, where the session is guaranteed non-nil. `installRoute` no longer touches `session.Context()`. No behavior change — every production handler still gets the cleanup exactly once via `newRelayHandler`.\n\n### Added\n\n- **Automation-friendly relay-chain benchmark suite (`internal/relay`, `scripts`, #284):** The relay-chain benchmarks emit machine-readable JSONL results (`BENCH_RESULTS_DIR`), including a 7-number latency summary (min/p25/median/p75/p95/p99/max) per config so the report can draw distribution plots. The fan-out sweep honors a `FANOUT_KS` env override. A new `TestRelayChain_ReconnectStorm` characterizes goroutine/heap behavior under subscriber churn (runs in the bench workflow via `RUN_STORM=1`, skipped in the per-PR CI gate). A zero-dependency Deno/TS report generator (`scripts/relay_bench_report.ts`) turns the JSONL into CSV + SVG plots: line charts with least-squares regression fits (per-hop latency slope, fan-out latency trend with R²), box-and-whisker plots of the latency distribution per K, a 4-panel overview (latency·loss·throughput·heap vs K), and a `derived.csv` of decision-grade numbers (per-hop ms/hop slope, fan-out knee K). One workflow runs it: `bench-relay.yml` (nightly full sweep K=1..128 + load + object-size + soak, plus an on-demand `workflow_dispatch` with a 30m/1h/3h/6h soak-duration choice). The per-PR CI integration gate skips the heavy `TestRelayChain_*` durability tests (`-skip='RelayChain'`); they run in the relay-bench workflow, their intended home.\n- **Route recovery on incumbent-end (`internal/relay`, #279):** A route-election loser is now retained as a per-`BroadcastPath` alternate instead of being cancelled, and is promoted to the active route when the incumbent's announcement ends. This fixes the publisher-mobility failure mode where a candidate rejected during the overlap was permanently discarded, leaving the path stranded once the incumbent was retracted. Promotion fires only on a definitive announcement-end (asynchronously, since `Announcement.end()` runs callbacks inline), so it introduces no route oscillation. At most one alternate is retained per path, kept by route quality (`isBetterRoute`) rather than recency, and promotion is serialized with route election under a single lock so a promotion can never clobber a freshly-elected route. New metrics: `qumo_relay_routes_retained`, `qumo_relay_route_promotions_total`. The robust fix for autonomous split-brain (two live publications coexisting without coordination) remains a future generation/epoch fence.\n- **Graceful migration / GOAWAY escape hatch (`internal/relay`, #280):** Wired `MOQDialer.OnGoaway` so a GOAWAY from an upstream peer relay is observed (`qumo_relay_peer_goaway_received_total{redirect}`) and logged instead of silently dropped, and plumbed `MOQServer.NextSessionURI` from the `GOAWAY_REDIRECT_URI` env so graceful shutdown advertises a redirect. GOAWAY is intentionally a session-level graceful-shutdown primitive (gomoqt exposes it as `Server.NextSessionURI` on `Shutdown` and `Dialer.OnGoaway`), which is what this wiring uses; publication relocation is handled by route/subscription migration (#279), not by GOAWAY.

### Changed

- **O(1) broadcast notification (`internal/relay`).** `trackDistributor` no longer fans out new-data notifications through a per-subscriber `[]chan struct{}` under an `RWMutex` — an O(N) channel-send loop on every delivered group. Replaced with `broadcastNotify`: an atomic sequence number plus a close-and-recreate channel, so `broadcast()` is O(1) with no per-subscriber state and no lock contention. Each egress goroutine reads an atomic `(seq, ch)` snapshot and detects new data by sequence comparison. Also closes a missed-wakeup window in `deliverGroup`'s wait: a notify landing while `wait()` read the channel fresh inside the select could previously delay an in-flight trickle frame until the next broadcast (data was never lost — `frames()` is level-triggered); a per-delivery `lastSeen` seq guard now mirrors the egress loop's check. New `BenchmarkBroadcastNotify_Listen` guards the read-side primitive.
- **Batched Prometheus egress counter (`internal/relay`).** `deliverGroup` now accumulates egress bytes in a local `int64` across a group's frames and flushes a single `Counter.Add` per group instead of one per frame, cutting atomic-CAS operations on the shared `metricRelayEgressBytesTotal` counter from O(frames) to O(groups) per subscriber delivery. The flush also runs on the mid-group `WriteFrame`-error path so bytes already handed to QUIC stay counted (metering-accurate). Per-session `addEgress` accounting remains per-frame — it is per-session, not a shared cross-subscriber counter, so it is not a contention source.

## [v0.4.0] - 2026-07-08

### Breaking Changes

- **Relay `ROLE` env var removed -> `--role` flag:** the node topology role is now `qumo relay --role hub|edge` (flag-only, no env fallback). Deployments setting `ROLE=...` must switch to the flag. Secrets and deployment config remain env vars.
- **SDN controller removed:** `qumo sdn` subcommand and all SDN-related packages (`internal/sdn`,
  `internal/topology`) have been removed. Cross-relay content discovery is now handled natively
  by moq-lite draft-03's ANNOUNCE_PLEASE mechanism.
- **config.sdn.yaml removed:** No longer needed. Relay-to-relay connectivity is configured via
  `peers` in `config.relay.yaml`.
- **ALPN changed from `moq-00` to `moq-lite-03`:** Peers must be upgraded together; mixed
  deployments with older versions are not supported.


### Added

- **`RTSPServer.Addr()` (`internal/ingest`):** exposes the bound listener address (nil before `ListenAndServe` binds), so callers and tests that configure `Addr: ":0"` can learn the actual port without reaching into unexported state.
- **Playground UI refinement — visual polish, UX, RTSP camera pull (`playground`):** Refined dark/light palettes with elevation tokens (`--shadow-sm/md`), card hover shadows, backdrop-blur stats overlay, smoother transitions. Scenario tabs renamed for clarity: "Echo" → "Webcam" (browser camera/screen), "Camera" → "IP Camera" (RTSP pull), "RTSP" → "RTSP Push"; each has a one-line description below the picker. New "IP Camera" scenario with a camera-URL input form that starts/stops an in-process RTSP pull client (`POST /api/pull`, `/api/pull/stop`, `/api/pull/status` on the playground server) and serves MoQT on `:4543`. The subscribe board only renders once the pull is active; before that a guided empty-state placeholder is shown. The pull's WebTransport dial is deferred until the pull is active (no spurious ERR_CONNECTION_RESET on page load).
- **Demo logging via `@okdaichi/media-log` (`playground`):** The playground now consumes the external `@okdaichi/media-log` library (jsr) instead of bare `console.*`. All call sites in `cert.ts`, `publish/media.ts`, `PublishBoard.tsx` (7), and `SubscribeBoard.tsx` (13) move to tagged, structured, level-filtered loggers (`createLogger`/`createMediaLogger` with `MediaTags`); errors are passed as structured fields (serialized in `exportLogs()` for bug reports) instead of positional args, and the `[Publish]`/`[Subscribe]` string prefixes become tags. The encode (publish) and decode (subscribe) frame loops also feed media meters — `meter.fps`/`meter.bitrate` and, on subscribe, `meter.gauge` for RTT and decode-queue depth — so the pipeline emits one diagnostic fps/bitrate/rtt/queue line per second alongside the existing UI overlay. Requires `@okdaichi/media-log@^0.1.0` on jsr.
- **RTSP pull ingest — connect IP cameras directly (`internal/rtsp`, `internal/ingest`):** `qumo rtsp <url> [path]` dials an RTSP source (DESCRIBE/SETUP/PLAY), receives interleaved RTP, depacketizes H.264/AAC, and republishes as MoQT — so an IP camera feeds MoQ natively without an ffmpeg bridge. Supports Basic + Digest auth (credentials in the URL), TCP-interleaved transport, automatic reconnect with backoff, and serves MoQT (WebTransport) so subscribers connect directly. The previous push-only `qumo rtsp` (ANNOUNCE/RECORD server) is now `qumo rtsp-push`. Also: `UnmarshalRTP` now correctly skips CSRC/header-extension and strips padding (needed for cameras that set those bits), and the SDP-media → track construction is factored into a shared helper used by both push and pull.
- **Demo live stats overlay (`playground`):** Both boards now show a real-time stats readout over the preview while a stream is active (#139) — resolution, fps, and media bitrate from a 1-second rolling meter (`stats.ts` `createStatsTicker`), plus encoder queue depth on publish and decoder queue depth + session RTT on subscribe (RTT from `session.getStats()`). The overlay is positioned out of the core video area, updates once per second, and clears on stop.
- **Demo actionable error messages (`playground`):** Publish/subscribe failures now surface as short, actionable messages instead of bare `Error: <opaque string>` (#138). A new `errors.ts` classifier maps the recognizable cases — denied camera/microphone access, no device, device busy, unsupported quality/codec, and MoQ subscribe failures (`TrackNotFound` → "No stream at this path yet", timeout, unauthorized) — to guidance that tells the user what to do; unrecognized errors fall back to a cleaned first line with no stack traces. `media.ts` no longer rewraps `getUserMedia`/`getDisplayMedia` errors (which dropped the `DOMException.name` the classifier keys on); the previously-uncaught encoder-config call in `PublishBoard` is now caught (releasing the acquired camera on failure); and subscribe errors that were `console.warn`-only now reach the UI. Connection-failure reasons are stripped of control characters and length-clamped.
- **Demo encode-quality + viewer controls (`playground`):** The publish board now exposes resolution (480p/720p/1080p), framerate (24/30/60), and bitrate (0.5–6 Mbps) picks that drive the camera capture and the encoder — stop and restart to apply a change (#135). The subscribe board gained mute, volume, and fullscreen viewer controls (#136) — pure client-side (WebAudio gain + Fullscreen API), no transport. Volume/mute use `AudioDecodeNode.gain` (it extends `GainNode`) and fullscreen uses the Fullscreen API on the video container; MoQ is live, so there is deliberately no pause/seek/scrub.
- **RTMP ingest codec init-data builders (`internal/ingest`):** `BuildAVCDecoderConfigurationRecord` and `BuildAudioSpecificConfig` serialize the parsed AVC/AAC configs into the codec initialization blobs a browser WebCodecs decoder expects as its `description` — the same shape the browser-publish path emits.
- **ffmpeg publisher driver supports RTSP (`internal/ffpub`):** `ffpub` now publishes to `rtsp://` URLs (forced TCP interleaving) as well as `rtmp://`, driving the RTSP interop test and any future RTSP push scenarios.
- **`qumo playground` subcommand (`internal/playground`, `main.go`, `embed.go`):** A one-command local demo. A single self-contained binary generates/caches a 14-day dev WebTransport cert, starts the relay in-process, serves the embedded web UI over HTTP on `127.0.0.1:8080`, and exposes runtime configuration to the browser via a `/config` endpoint. The cert hash moves from a build-time `VITE_CERT_HASH` constant (which forced rebuilding the UI on every cert change) to a runtime `/config` fetch, with the existing `import.meta.env` values retained as a fallback so the `mage web` Vite dev workflow is unchanged. The UI is embedded via `//go:embed all:playground/dist` at the repo root (embed paths can't traverse `..`); `mage build` runs `mage webbuild` first so `bin/qumo` ships with the UI baked in, and a committed placeholder `index.html` keeps `go build` / `go install` working on a fresh clone before the UI is built. There is deliberately no `--host` flag: the browser-facing relay URL is derived per-request from the host the UI was opened at (`X-Forwarded-Host` honored behind a reverse proxy), so public hosting behind a TLS-terminating proxy needs only `--relay-addr 0.0.0.0:4433` — the dev cert is pinned by SHA-256 hash, so it works on a public host without regeneration. Only `--ui-addr` / `--relay-addr` (bind addresses) are flags. The app name is a fixed `qumo` (the former `VITE_APP_NAME` env var was dropped).
- **Demo publish source switcher (`playground`):** The publish board's media-source picker is now a segmented toggle (Camera with a camera glyph / Screen with a monitor glyph — icons from the public `lucide-solid` icon set, stroked with `currentColor`) instead of a dropdown, matching the scenario-picker style and making the two sources discoverable at a glance. The segmented-control styling was promoted to a shared `.segmented`/`.segmented-btn` class reused by the scenario picker, and the "Streaming from" status line now shows the friendly label ("Camera"/"Screen") rather than the raw signal value. The switch is disabled while streaming (stop to switch sources).

### Changed

- **Playground pull API is now testable (`internal/playground`):** the RTSP-pull handlers (`/api/pull`, `/api/pull/stop`, `/api/pull/status`) are now backed by a `pullHandle` interface + an injectable `pullStarter` (production default unchanged — `ingest.PullAndServe`), so they can be exercised without binding a real QUIC listener or presenting a cert. Internal refactor; no behavior or public-API change.
- **Relay `--role` flag replaces the `ROLE` env var (`internal/relay`, `main.go`):** the node topology role is an execution mode, not deployment config, so it is now a discoverable flag — `qumo relay --role hub` (or `edge`; omit for a flat / single-node relay). The `ROLE` environment variable is **removed** (no fallback) to avoid two sources of truth and the misconfiguration that brings — this is a breaking change for deployments that set `ROLE=…`; switch to `relay --role …`. Secrets and deployment configuration (tokens, certificates, `RELAY_ADDR`, `PEERS`, …) remain env vars. `qumo relay --help` prints the flag summary; positional args are rejected so `qumo relay hub` does not silently mean nothing. Docker / README examples updated.
- **Relay capacity knobs now take effect (`internal/relay`):** `GROUP_CACHE_SIZE` and `FRAME_CAPACITY` were read into `Config` but never consumed — the per-track group ring and frame pool were hardcoded to `DefaultGroupCacheSize` (8) and `DefaultFramePool`. They are now wired through the track manager: a positive `GroupCacheSize` sizes each track's ring, a positive `FrameCapacity` mints a per-node `FramePool`; ≤0 / unset fall back to the defaults. The `GROUP_CACHE_SIZE` default in `relay-config.example.env` is corrected from 100 (the value the relay was ignoring) to 8 to match actual behavior. The tautological `config_test.go` (which asserted struct-field round-trip, not behavior) is replaced with tests of the default-resolution logic.
- **Removed unused relay `REGION` config (`internal/relay`):** `Config.Region` / the `REGION` env var were logged at startup but never consulted — routing is role-based (`ROLE`), and the resolver-side `Region` on discovered peers (a separate field) is also unread. The field, env read, startup log line, and `relay-config.example.env` / `docker/README.md` references are removed. (Region-based routing can be re-added when actually implemented.)
- **Dependency updates — minor/patch (`go.mod`, `playground`):** Bundled demo/frontend and Go module dependency refresh, in-range minor/patch only. Frontend (`@qumo/moq` 0.16.1 → 0.16.2, `solid-js` 1.9.12 → 1.9.14, `@types/node` 25.6 → 25.9, `vite` 7.3.2 → 7.3.6) and Go indirects (`prometheus/common`, `prometheus/procfs`, `golang.org/x/net`, `golang.org/x/text`, `google.golang.org/protobuf`, et al.). `go mod tidy` dropped the unused `go.yaml.in/yaml/v2`. Build, type-check (`deno check`), and the full test suite pass. Major-version bumps (`typescript` 6, `vite` 8, `@types/node` 26) are tracked separately.

### Fixed

- **`internal/loadgen` capacity tally is now race-free (`runCarry`).** The per-session `connected []bool` / `receiving []int` slices were written by the subscriber goroutines and read by the main goroutine after `drain(&wg, 20s)` — but `drain` returns on its safety timeout too, so a subscriber wedged past the deadline (despite `subCancel`) could still be writing its slot while the main goroutine read it (a `-race` data race, latent because the timeout essentially never fires). Replaced the two shared slices with `atomic.Int64` tallies (`connCount`, already the ramp-settle signal, doubles as the connected count; a new `receivingCount`), so the post-drain read is defined regardless of a straggler. `subscribeOne` now returns just the frame count (connected is tracked via `connCount`). No behavior change to the reported numbers.
- **Playground pull API validates URL + broadcast path (`internal/playground`):** `/api/pull` now rejects URLs that are not `rtsp://`/`rtspd://` (or lack a host) and broadcast paths that aren't a `/`-prefixed, URL-safe-charset, length-bounded string — defense-in-depth against SSRF and log-injection (moqt only requires a leading `/`, so a path with spaces/control chars/shell metacharacters would otherwise be logged verbatim and used as a routing key). Private/LAN hosts remain intentionally allowed — pulling from an IP camera on the local network is the feature's primary use case. The playground is a local dev tool; its `/api/pull` must not be reachable in a publicly-hosted deployment.
- **Embedded version no longer carries a `-dirty` suffix (`magefiles`, `playground`):** `mage build` computed the git version via `git describe --dirty` *after* `WebBuild`, which overwrites the committed `playground/dist/index.html` placeholder on every build — so the version baked into the binary (and reported by `qumo version`) always carried `-dirty`, which would have shipped in the release string (e.g. `v0.4.0-dirty`). The version is now captured *before* `WebBuild`, so a build from a clean tag checkout yields a clean `v0.4.0`. `playground/deno.lock` was also under-resolved (the `av-nodes@0.10.4` entry was missing its `dependencies` edge), so every Deno run re-added it and dirtied the tree — the committed lock now matches what Deno produces.
- **Quieter `mage web` dev console (`playground`):** `config.ts` no longer fires a `GET /config` 404 on every Vite-dev load (the dev server doesn't serve `/config`; only the built `qumo playground` binary does) — it skips the fetch when `import.meta.env.DEV` and reads `import.meta.env` directly, with the built UI unchanged. Also removed the left-in first-10-frames hex-dump diagnostic (`[Subscribe] video #N … hex=[…]`) that flooded the console on each subscribe.
- **Subscribe-to-empty-path no longer misreports a relay connection failure (`playground`):** when the relay reset a subscribe stream before responding and the MoQ error code wasn't carried, the demo surfaced the raw `WebTransportError: Received RESET_STREAM` and — because that string contains "WebTransport" — mis-classified it as *"Could not connect to the relay."* The relay was reachable; the path just had no publisher. `errors.ts` now recognizes a subscribe-context stream reset and maps it to the same actionable *"No stream at this path yet. Make sure the publisher — or the RTMP/RTSP pusher — is running, then click Start again."* text used for `TrackNotFound`.
- **Cross-origin WebTransport rejections are now logged (`internal/cors`):** `NewChecker` silently rejected browser requests whose `Origin` wasn't on the allow-list, so a misconfigured `CORS_ALLOWED_ORIGINS` surfaced only as a browser-side `ERR_CONNECTION_RESET` with no server-side trace — making the common "browser can't connect" cause hard to diagnose. The checker now emits a `slog.Info` on rejection with the origin, the request host, and a remediation hint. (Accept/reject behavior is unchanged.)
- **RTSP-ingested streams no longer play back with a broken picture and audio pops (`internal/ingest`):** Two independent RTSP ingest defects made RTSP playback unwatchable (corrupted/tearing video and clicking audio) where the same `Session`/player played RTMP fine. **Video:** ffmpeg's RTSP muxer emits several IDR NALUs at the same RTP timestamp within one access unit; ingest pushed one MoQT frame per NALU, so a keyframe produced several same-PTS frames in one group, only the first of which the player marks `key` — the rest were fed to WebCodecs as `delta` chunks at an identical timestamp and decoded as competing pictures. H.264 depacketization now aggregates every NALU sharing an RTP timestamp into one AVCC sample (one access unit → one frame), with the boundary detected by RTP-timestamp change plus the marker bit, and now also splits STAP-A aggregation packets (previously dropped). **Audio:** an mpeg4-generic RTP packet packs 3–4 AAC access units; ingest pushed each as its own MoQT group, so one packet burst N concurrent QUIC streams (MoQT maps a group to a stream) that gomoqt delivers in stream-arrival order — the subscriber received AAC frames out of PTS order and the decoder popped. `Session.PushAudioFrames` now coalesces one packet's access units into a single multi-frame group, keeping them on one stream in order. Regression guard: `TestRTSPPlayback_FrameIntegrity` (integration) asserts video frames have unique PTS and audio frames arrive monotonically; unit tests cover STAP-A splitting, access-unit aggregation, and the coalesced-audio group path.
- **Demo no longer shows a false "WebTransport will reject" cert warning under mkcert (`playground`):** `ConnectionStatus` warned whenever `VITE_CERT_HASH` was unset — exactly the state the mkcert path creates by clearing it, since the cert is trusted via the local root CA and needs no pin. `cert.ts`'s `buildTransportOptions` now treats a missing hash as a non-problem (only a malformed hash is flagged), so mkcert users no longer see the wrong remediation; a genuinely-forgotten self-signed cert still surfaces via the connection-error path.
- **`qumo playground` rejects an un-pinnable shared cert (`internal/playground`):** `loadCertIfFresh` now refuses a cached cert whose validity exceeds the 14-day WebTransport `serverCertificateHashes` limit (e.g. a mkcert cert pulled in via `QUMO_PLAYGROUND_CERT_DIR=certs`) instead of serving a hash Chrome would reject — and without clobbering the shared file. The error tells the user to unset `QUMO_PLAYGROUND_CERT_DIR` or use a ≤14d cert.
- **RTSP ingest no longer creates spurious PTS regressions from redundant same-timestamp IDRs (`internal/ingest`):** ffmpeg's RTSP muxer intermittently emits several IDR NALUs back-to-back at the same presentation timestamp within one access unit. The ingest opened a fresh MoQT group on every keyframe NALU, so this produced rapid micro-group churn that the relay ring / a bounded collector window delivered out of order — observed downstream as a deterministic ~1.97 s backward PTS jump at a GOP boundary that intermittently flaked `TestRTSPInterop_Matrix/gop60_720p30` on CI (#229). `videoTrack.push` now opens a new group only when a keyframe's timestamp differs from the group being filled, collapsing same-AU IDRs into one group. For all well-behaved streams (one IDR per access unit, distinct timestamps) behavior is unchanged; the RTMP path (one access unit per push) is unaffected. The matrix case's widened-stopgap `MaxCTSWindowUS` is reverted to the shared 1 s window, and a wide-net `TestRTSP_PTSMonotonic` guard is added.
- **`qumo playground` now connects from the browser out of the box (`internal/playground`, `internal/cors`):** The one-command demo serves the UI and the relay on different ports, so the browser's WebTransport was cross-origin and rejected by the same-origin default (the same root cause #224 fixed for `qumo relay`). `configureRelayEnv` now defaults `CORS_ALLOWED_ORIGINS=same-host` when unset. `internal/cors` gained a `same-host` allow-list entry (port-insensitive host match) that fits playground's design exactly — the relay URL is derived per-request from the browser's own Host, so `Origin.Host` and the relay `Host` always share a host; a different host is still rejected. An explicit `CORS_ALLOWED_ORIGINS` value is respected. Closes #225.
- **Relay now honors `CORS_ALLOWED_ORIGINS` for browser WebTransport (`internal/relay`, `internal/cors`):** The main relay's `WebTransportHandler` left `CheckOrigin` unset, so it fell back to webtransport-go's same-origin-only default — meaning no browser could connect to the relay when the UI was served from a different origin (the entire local `mage web` Vite dev workflow, and any multi-origin deploy). The Go interop client passed only because it sends no `Origin` header. The standalone RTMP/RTSP ingest servers already read `CORS_ALLOWED_ORIGINS` (#141); that logic is now extracted into a shared `internal/cors` package (`LoadAllowed` + `NewChecker`, secure same-origin default, `*` opt-out) used by **both** the relay and the ingest servers, and the relay wires it into its `WebTransportHandler`. Default behaviour is unchanged (same-origin only); set `CORS_ALLOWED_ORIGINS=http://localhost:5178` (etc.) to allow the dev UI's origin.
- **Playground production UI build repaired (`playground`):** `npm run build` / `mage webbuild` was broken by two pre-existing issues. (1) `@deno/vite-plugin` 1.0.6 crashed under vite 7 / deno 2.8 (`resolveViteSpecifier: Cannot read properties of undefined (reading 'startsWith')`) — upgraded to `^2.0.2`, which supports vite 5–8 and current deno, so `vite build` now succeeds. (2) the build's `tsc -b` type-check could not resolve jsr imports (`@qumo/moq`, `@okdaichi/*`) because standalone `tsc` ignores `deno.json`'s import map; the build script now type-checks via `deno check src` (which resolves jsr and passes cleanly) before `vite build`.
- **RTSP ingest now works with ffmpeg (`internal/ingest`):** Two protocol bugs surfaced by the RTSP interop test, either of which aborted every ffmpeg RTSP publish: (1) the SETUP handler parsed the Transport header with a strict `Sscanf("RTP/AVP/TCP;interleaved=%d-%d")` that rejected ffmpeg's actual `RTP/AVP/TCP;unicast;interleaved=0-1` (extra `;unicast;`) with 400 Bad Request — now parsed robustly via `parseInterleavedChannels`; (2) AAC track detection was case-sensitive (`mpeg4-generic`) while ffmpeg emits `MPEG4-GENERIC` — codec detection is now case-insensitive, and an empty SDP `a=control` no longer matches every SETUP.
- **Relay no longer panics without `REMOTE_RESOLVER_URL` (`internal/relay`):** `relay.Run` called `remoteResolver.Interval()` unconditionally, but `NewRemoteResolver` returns nil when `REMOTE_RESOLVER_URL` is unset (the common single-node/demo case), panicking at startup. The resolver is already treated as optional by its consumer (`server.go`), so the call is now guarded. This also unblocks `qumo playground`, which starts the relay without a remote resolver.
- **Ingested AAC audio now plays in the demo subscriber (`playground`):** RTSP/RTMP ingest publishes AAC with its AudioSpecificConfig (ASC) as Base64 catalog `initData`, but the subscribe path decoded `initData` into the WebCodecs `description` only for video — the `AudioDecoder` was configured with just `codec`/`sampleRate`/`numberOfChannels` and no ASC, so raw AAC frames (no ADTS, as ingest emits) failed to decode and ingest audio was silent while video played fine. `SubscribeBoard` now Base64-decodes `audioTrack.initData` into the `AudioDecoder` `description`, mirroring the video path.

### Changed

- **Major-version frontend dependency bumps (`playground`):** `typescript` 5.9 → 6.0, `vite` 7.3 → 8.1, `@types/node` 25 → 26. Type-check (`deno check`) and the vite production build pass with the new majors. Split out from the minor/patch refresh (#267) so the major jumps are evaluated in isolation.
- **`mage relay` + `mage web` now connect out of the box; relay defaults to a dual-stack bind (`internal/relay`, `magefiles`, `playground`):** the relay's default `RELAY_ADDR` changed from `0.0.0.0:4433` to `:4433`, which binds both IPv4 and IPv6 — so `https://localhost:4433` works on hosts where `localhost` resolves to `::1` (e.g. Windows), which previously reset the browser's WebTransport handshake. The `mage relay` dev wrapper now also applies dev-friendly defaults when unset — `ADVERTISE_ADDR=localhost:4433` (required for the wildcard bind) and `CORS_ALLOWED_ORIGINS` allowing the Vite dev UI origins — so `mage relay` alongside `mage web` no longer needs manual env setup. User-set env always wins; the standalone `qumo relay` binary keeps its secure same-origin CORS default. Refs #234.
- **`mage cert` prefers mkcert for local dev (`magefiles`, `playground`, `docker`):** `mage cert` now signs the localhost WebTransport cert with [mkcert](https://github.com/FiloSottile/mkcert) when it's on PATH, producing a long-lived cert that chains to a trusted local root CA — so the browser trusts it directly and no `VITE_CERT_HASH` pinning, no 14-day re-run, and no Vite restart are needed. `mkcert -install` runs first; if it fails (e.g. the Linux system store needs root) `mage cert` falls back to the self-signed path rather than signing an untrusted cert and wiping the working `VITE_CERT_HASH` pin. A stale `VITE_CERT_HASH` (and its comment) from a prior self-signed run is cleared from `playground/.env` so it can't pin the wrong cert. A new `CERT_HOSTS` env var (comma-separated, mkcert path only) appends extra SANs beyond the default `localhost`/`127.0.0.1`/`::1`, so the cert also validates when the demo is reached from another device on the LAN (e.g. `CERT_HOSTS=192.168.1.10,desktop.local mage cert`). When mkcert is absent (CI, headless, air-gapped), `mage cert` falls back to the previous 14-day self-signed ECDSA cert and writes `VITE_CERT_HASH` as before, so those workflows are unchanged. The self-signed key is now written at 0600 (was world-readable). Closes #196.
- **Release artifacts now embed the real playground UI (`playground`, `docker`, `.github`):** goreleaser and the Docker image build previously ran `go build` from a fresh checkout, so `//go:embed all:playground/dist` embedded only the committed placeholder and `qumo playground` shipped the "UI not built" page. goreleaser now builds the UI in a `before` hook (`cd playground && deno install && deno task build`), with a `denoland/setup-deno` step added to `release.yml`; the Dockerfile gained a `node:22-alpine`-based frontend stage (Deno installed for jsr/`deno check`, Node for Vite) whose `dist` is overlaid into the Go build stage. `.dockerignore` now admits the playground source (excluding `node_modules`, host `dist`, `.env`) so the frontend stage can build. Tagged releases and the published image now serve the real demo UI.
- **RTMP/RTSP ingest forward AVCC unchanged (`internal/ingest`):** RTMP ingest no longer converts AVCC to Annex-B — video frames are passed through as AVCC (length-prefixed NALUs) with the codec string switched from `avc3` to `avc1`. RTSP ingest was moved onto the same format (NALUs are now AVCC-length-prefixed via `wrapAVCC` instead of Annex-B start codes, and its config sets `NALULenSize: 4`). Video/audio catalog tracks now carry Base64-encoded `initData` built from the sequence header. This conforms both ingest paths to the same MoQT wire format the browser-publish path emits, making ingested streams browser-decodable. `AVCCToAnnexB` and its tests were removed; PTS = DTS + CTS preserves B-frame timing via `parseFLVVideoCTS`.

- **Bumped gomoqt to v0.16.0 (`go.mod`):** Upgraded the MoQT library from v0.15.0. Notable upstream fixes carried in by this release include a critical OOM denial-of-service fix for unconstrained varint allocation, `Server.Close()` no longer hanging with active connections, and `OpenGroup`/`OpenGroupAt` backpressuring on the QUIC uni-stream limit instead of aborting. `OpenGroup`/`OpenGroupAt` now require a `context.Context`; call sites in `internal/ingest`, `internal/relay`, and `internal/smoketest` were updated accordingly.
- **Bumped gomoqt to v0.16.1 (`go.mod`):** Carries an updated `webtransport-go` fork (v0.11.0-okdaichi.1) that fixes a `Session.CloseWithError` deadlock — the close blocked on an internal `sync.WaitGroup` waiting for stuck stream goroutines, hanging the interop test matrix ~50% of runs on Windows. The RTMP interop matrix is now 10/10 stable (was ~50% pass). Closes #205.
- **groupCache concurrency model — RCU / copy-on-write (`internal/relay`):** Replaced the slice + atomic-length lockless-read scheme (which carried a benign data race on the slice header, kept non-fatal only by the never-shrink invariant) with an `atomic.Pointer`-published immutable-snapshot design. `append` is now copy-on-write via a compare-and-swap loop (safe under concurrent writers); `next` loads an immutable snapshot — reads stay lock-free and zero-allocation (~0.29 ns/op, unchanged) and are now data-race-free under the Go memory model. Trade-off: appends become O(n) copy-on-write (higher write cost) in exchange for formal race-freedom and the ability to safely reset a live cache. Removed the now-unneeded `sync.RWMutex` and `frameLen` fields.
- **Web demo directory renamed `solid-deno` → `playground`:** The relay's browser demo / test client was named after its original tech stack (SolidJS + Deno); renamed to `playground` to describe its role. All references updated — the magefile `web` / `webBuild` / `cert` targets and path literals, the docker compose demo config, `README.md`, `.dockerignore`, and the in-app `package.json` name — with full file history preserved via a pure `git mv`. Historical mentions of `solid-deno` elsewhere in this changelog are intentionally left as-is.
- **Bundled demo deps (`playground`):** `@okdaichi/av-nodes` 0.10.3 → 0.10.4 (the encode loop stops instead of spinning when `encode()` throws on an unconfigured codec) and `@okdaichi/golikejs` 0.9.0 → 0.10.0. `@okdaichi/media-log` was already at the latest in range.

### Performance

- **Relay Handler Egress Allocation Optimization:** Extracted `string(tw.TrackName)` conversion outside the wait loop in the track distributor egress handler, preventing unnecessary memory allocations in the tight loop.

### Removed

- **Bootstrap server removed (`internal/bootstrap`):** The bootstrap discovery server
  and client (`qumo bootstrap` command) have been removed from this repository.
  Bootstrap functionality with traffic engineering is being migrated to a
  separate control plane service.
- **Removed stale `examples/web-demo/`:** An orphan README pointing at a defunct JSR-based demo; the live web demo lives in `playground/` (formerly `solid-deno/`), where all `mage web` targets already pointed.

### Security

- **RTMP listener hardened against handshake stalls and bad clients (`internal/rtmp`):** `Listener.Accept` now runs the RTMP handshake under a read deadline (default 10s), so a client that connects and then stalls can no longer hold the accept loop and block every other RTMP connection. Handshake failures (a stalled, half-open, or otherwise-misbehaving client) are closed and skipped instead of returned as an Accept error — previously a single failed handshake took down the whole ingest server, since server accept loops treat any Accept error as fatal. Skipped handshakes are logged at debug level for observability.
- **Bumped `golang.org/x/crypto` to v0.53.0 (`go.mod`):** Clears a set of `golang.org/x/crypto/ssh` HIGH-severity CVEs (CVE-2026-39829, -39830, -39832, -39835, -42508, -46595, -46597) present in the previously-resolved v0.51.0, which the SHA-pinned Trivy image scan now reports end-to-end — that scan only became functional once `docker.yml` builds are loaded into the local Docker daemon. `govulncheck` confirms the vulnerable `ssh` package is not reached by qumo's code, but bumping the module removes the finding at the source. Also pulls `golang.org/x/sys` → v0.46.0 and `golang.org/x/text` → v0.38.0.
- **CORS origin check hardened (`internal/ingest`):** `WebTransportHandler.CheckOrigin` no longer accepts every origin for the RTMP and RTSP ingest servers. Origins are validated against a comma-separated `CORS_ALLOWED_ORIGINS` environment variable (supporting a `*` wildcard), with a same-origin fallback, closing a WebTransport cross-site request forgery risk.
- **TLS configuration hardened (`internal/relay`):** Removed `InsecureSkipVerify` from the relay dial
