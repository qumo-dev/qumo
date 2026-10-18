---
title: funnel
description: Gather many HTTP senders' messages into one MoQT track.
weight: 5
---

Gathers many HTTP senders into one MoQT track. Senders POST messages to a
track's URL; the funnel orders them into the track, one group each, names
each one's sender from its credential, and publishes the track as MoQT. Each
record is committed to a [qumo-ledger](https://github.com/okdaichi/qumo-ledger)
track before it is published. That ledger is in memory by default, or durable
with `LEDGER_URI`, and it also serves the track's history. The funnel
publishes through a relay it dials as a client, so subscribers reach its
tracks on the relay they already use; it can also serve them on a listener of
its own. It does not join the relay mesh as a peer.

It suits small, frequent messages from many senders that subscribers want as
one stream, such as chat, reactions or presence.

## Usage

```
qumo funnel
```

Takes no flags or arguments — configured entirely through environment
variables.

## Example

```console
$ RELAY_URL=moqt://relay:4433 RELAY_SIGNING_KEY=funnel-key.jwk RELAY_PUBLISH=room \
  LEDGER_URI=file:///var/lib/qumo QUMO_AUTH_KEYS=keys.json qumo funnel
	Ingest  : :8090
	Relay   : moqt://relay:4433
	Serve   : none
	Ledger  : file:///var/lib/qumo
	Auth    : keys.json
```

Without `LEDGER_URI`, records are kept in memory and are lost when the
process exits. Without `QUMO_AUTH_KEYS`, every sender is accepted.
Without `RELAY_URL`, the funnel serves subscribers itself on `:4433`.

The app creates a track by putting to its URL, `/tracks/` followed by the
broadcast path and the track name, and senders post records into it:

```console
$ curl -X PUT localhost:8090/tracks/room/123/chat -H "Authorization: Bearer $APP"

$ curl -X POST localhost:8090/tracks/room/123/chat \
    -H "Authorization: Bearer $ALICE" \
    -H "Idempotency-Key: 7f9c2b" \
    -d '{"text": "hello"}'
{"group":"e000001-g00000000","wallclock":1791370000000000000}

$ curl localhost:8090/tracks/room/123/chat?limit=50 -H "Authorization: Bearer $VIEWER"
{"records":[{"group":"e000001-g00000000","wallclock":1791370000000000000,"sender":"alice","payload":{"text":"hello"}}]}
```

Subscribers consume the track `chat` of the broadcast `/room/123`. Any number
of senders post into the same track, and a subscriber receives all of them
on one subscription.

## Requests

| Endpoint | Body | Result |
|---|---|---|
| `PUT /tracks/{broadcast path}/{track name}` | — | Creates the track and publishes it, so subscribers can wait on it before its first record: `201`, or `204` when it exists. |
| `POST /tracks/{broadcast path}/{track name}` | The record's payload, one JSON value in UTF-8 | `201` once the record is committed; `404` when the track does not exist. A record never creates its track. |
| `GET /tracks/{broadcast path}/{track name}` | — | A page of records, oldest first: the newest, or those before `?before=<group>`, at most `?limit=` (default 50, at most 200). `before` in the answer is the cursor for the next older page. A redacted record keeps its `group` and `wallclock` with `"redacted": true`, and has no sender or payload. |
| `DELETE /tracks/{broadcast path}/{track name}?group=<group>` | — | Redacts that record ([qumo-ledger](https://github.com/okdaichi/qumo-ledger) `ingest`): commits and publishes a redaction, `{"redacts": "<group>"}`, then deletes the record's payload. `201` with the redaction's group, or `204` when the record was redacted already; `400` for a missing or malformed `group` or a redaction itself, `404` for no such track or group. Without `QUMO_AUTH_KEYS` it is open to anyone, like every other request. |

The last segment of the URL is the track name; the segments before it are the
broadcast path.

Each record is stored as `{"sender": …, "payload": …}`: the payload as it was
posted, and the sender its credential names (below). A record whose
credential names no sender has no `sender`.

A record sent with an `Idempotency-Key` header is stored once per track: a
retry with the same key is answered as the first was and stores nothing. A
track remembers its 1024 most recent keys.

With `FUNNEL_SENDER_LIMIT` and `FUNNEL_TRACK_LIMIT`, records are limited per
sender and per track; past either, a record is answered `429` with a
`Retry-After`.

## Credentials

With a key set (`QUMO_AUTH_KEYS`), every request carries a qumo credential as
`Authorization: Bearer`, the same kind the relay admits sessions with and
verified against the same key set. Its root and each action's path patterns
([auth](../auth/#what-a-token-grants)) name the broadcasts it reaches, and
each reaches every track of them:

- **Creating** a track (`PUT`) needs a `create` pattern matching the broadcast
  path; the funnel then publishes the track. It is for the app, which
  decides which tracks a broadcast has; a sender's credential has none, so
  senders record only into the tracks the app made. A `publish` pattern, which
  sends through a relay, creates nothing here, so a publisher can't make
  funnel tracks under its own broadcast.
- **Posting** (`POST`) needs a `post` pattern matching the broadcast path. The
  record's sender is the credential's `sub`, whatever the payload claims; a
  credential without `sub` posts with no sender. A `publish` pattern does not
  post, and a `post` pattern never publishes through a relay, so a sender's
  credential can't publish into the relay directly.
- **Reading** history (`GET`) needs a `subscribe` pattern matching the
  broadcast path, as a relay's FETCH does.
- **Redacting** (`DELETE`) needs a `redact` pattern matching the broadcast
  path. The redaction carries the credential's `sub` as its
  sender. No other action redacts, so a sender's `post` credential can't take
  anyone's record out; an app that lets senders redact their own records
  checks the record and redacts with a credential it keeps to itself.
  Redactions are published live and kept in history, so a `sub` there is
  visible to every subscriber; sign redact credentials without `sub` to keep
  moderators anonymous.

```json
{"root": "/room/", "sub": "alice",
 "post": ["/123/comments"], "subscribe": ["/"],
 "iat": 1791370000, "nbf": 1791370000, "exp": 1791370600}
```

posts as `alice` into the tracks the app made of `/room/123/comments`, and
reads any track of every broadcast beneath `/room`. A wildcard grants one
pattern for many broadcasts: `"post": ["/{room}/comments"]` posts into the
comments of every room.

A `path_auth` credential is read as the grant it amounts to: its `sub` reads
any track at or beneath its path. Its `pub` is publishing, so it creates
and posts nothing.

The credential is checked on every request: once it expires or its key leaves
the key set, it is refused. A key paused with `"pause": ["publish"]` creates no
track, and its credentials still post, read and redact. A missing or invalid credential answers `401` with
`WWW-Authenticate: Bearer`, and one that does not cover the broadcast `403`.

## Publishing through a relay

With `RELAY_URL`, the funnel dials the relay as a client and announces its
broadcasts on that session; the relay subscribes to them when its subscribers
do. The session needs a credential that may publish there. The funnel either
uses the `?jwt=` in `RELAY_URL`, or, with `RELAY_SIGNING_KEY` and
`RELAY_PUBLISH`, signs a fresh one for every session, whose root is
`RELAY_PUBLISH` (a directory, `/room/`; a bare `room` reads the same) and
which grants `publish` on every path beneath it (`"publish": ["/"]`), from a
key the relay's key set trusts (`qumo auth keygen`, ideally confined with
`-prefix`). A track is then created only beneath `RELAY_PUBLISH`, where the
relay takes the funnel's announcements: a `PUT` for one elsewhere, or at
`RELAY_PUBLISH` itself, is answered `403`. A credential lasts at most an
hour, and what it grants with it, so the funnel presents a freshly signed one
on the session before the one in force expires, and dials again if none is
granted in time. One in `RELAY_URL` can't be renewed: the session publishes
nothing after it expires, so a long-running funnel signs its own.

When a session ends, the funnel dials again after two seconds and announces
its broadcasts anew.

## What a subscriber receives

Each record is one group holding one frame, the record as it was stored,
`{"sender": …, "payload": …}`. A record is sent only after it is committed, in
commit order. A group's
sequence is one more than the sequence of the ledger group that stores the
record. A new subscriber starts at the track's latest record.

A subscriber reaches a track once it has been created, by its first record or
by a PUT. A subscription to any other track name of the broadcast is refused
as not found.

At startup the funnel lists its store once and publishes every track it
recorded before, with its latest record, so subscribers reach them across a
restart before anyone records again.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `FUNNEL_ADDR` | `:8090` | HTTP listen address for records and history. |
| `FUNNEL_SENDER_LIMIT` | `1,5` | Records each sender may send into a track, as `RATE,BURST`: per second, and at once. `0` or `off` is no limit. |
| `FUNNEL_TRACK_LIMIT` | `20,40` | Records a track takes from all senders together, as `RATE,BURST`. `0` or `off` is no limit. |
| `RELAY_URL` | (unset) | The relay to publish through, dialed as a client: `https://relay:4433` for WebTransport or `moqt://relay:4433` for native QUIC. It may carry a credential as `?jwt=`. |
| `RELAY_SIGNING_KEY` / `RELAY_PUBLISH` | (unset) | A signing key file and the root its credentials grant publishing beneath, e.g. `/room/` for `/room/123/comments`. Set together; the funnel signs a fresh relay credential for every session in place of `?jwt=`, and renews it on the session before it expires. |
| `RELAY_CA_FILE` | (unset) | PEM certificate to trust as the relay's root, instead of the system roots. |
| `RELAY_TLS_INSECURE` | `false` | Skip relay TLS verification, for a self-signed dev relay. |
| `FUNNEL_SERVE_ADDR` | `:4433` without `RELAY_URL`, none with it | MoQT listen address for subscribers that dial the funnel directly. |
| `LEDGER_URI` | (unset) | Where records are stored. The scheme selects the backend: `file:///var/lib/qumo` is a directory (`file:ledger` for a relative one); `postgres://user@host:26257/qumo` is a table (`ledger_objects`, or `?table=`) in PostgreSQL or CockroachDB; `s3://bucket/prefix?region=…` is a bucket of S3, or of an S3-compatible service with `&endpoint=http://host:9000`, with credentials from `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`; unset or empty is memory. A bare path or any other scheme is an error. |
| `CERT_FILE` / `KEY_FILE` | `certs/server.crt` / `certs/server.key` | TLS certificate and key for the funnel's own MoQT listener. |
| `CORS_ALLOWED_ORIGINS` | (unset) | Comma-separated origins allowed to call the HTTP endpoints from a browser and to open WebTransport (default: same-origin only; `*` allows any). |
| `QUMO_AUTH_KEYS` | (unset) | The key set senders' and readers' credentials are verified against, as the relay reads it: a file, or an https URL. Unset accepts every request, with no sender. |
| `QUMO_AUTH_KEYS_CACHE` | (unset) | A file a downloaded key set is kept in between runs. |
| `QUMO_RELAY_TOKEN` | (unset) | Bearer token sent to a key-set URL. |

## Limits

- **Plain HTTP.** The HTTP listener does not terminate TLS; put it behind
  something that does, since credentials travel in its requests.
- **Memory is not bounded.** With no `LEDGER_URI`, every record stays in
  memory until the process exits. Use it for development.
- **One process per track.** Records of a track are ordered within one process;
  two processes recording into the same track are not coordinated.
- **Idempotency keys do not survive a restart.** A retry that reaches a
  restarted funnel is stored again.
- **A relay session ends when its credential does.** Subscribers through the
  relay see the track end and subscribe again when the funnel redials, at most
  hourly with a signed credential.
- **A quiet track answers late.** A subscription to a track with no record yet
  is answered when the first record arrives.
- **Recorded tracks are not served over HLS.** Records carry no duration, which
  the [hls]({{< relref "hls" >}}) renderers need.

## See also

- [relay]({{< relref "relay" >}}) — the relay the funnel publishes through.
- [rtmp]({{< relref "rtmp" >}}) — ingest media over RTMP.
- [hls]({{< relref "hls" >}}) — the other command built on qumo-ledger.
