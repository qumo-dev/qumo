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

A sender posts to the track's URL, `/tracks/` followed by the
broadcast path and the track name:

```console
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
| `POST /tracks/{broadcast path}/{track name}` | The record's payload, one JSON value in UTF-8 | `201` once the record is committed. Creates the track when it does not exist. |
| `PUT /tracks/{broadcast path}/{track name}` | — | Creates the track ahead of its first record, so subscribers can wait on it: `201`, or `204` when it exists. |
| `GET /tracks/{broadcast path}/{track name}` | — | A page of records, oldest first: the newest, or those before `?before=<group>`, at most `?limit=` (default 50, at most 200). `before` in the answer is the cursor for the next older page. A redacted record keeps its `group` and `wallclock` with `"redacted": true`, and has no sender or payload. |
| `DELETE /tracks/{broadcast path}/{track name}?group=<group>` | — | Redacts that record ([qumo-ledger](https://github.com/okdaichi/qumo-ledger) `ingest`): commits and publishes a redaction, `{"redacts": "<group>"}`, then deletes the record's payload. `201` with the redaction's group, or `204` when the record was redacted already. |

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
verified against the same key set. Its scopes
([auth](../auth/#what-a-token-grants)) name the tracks it reaches:

- **Posting** (`POST`, `PUT`) needs a `post` scope matching the broadcast
  path and the track name. The record's sender is the credential's `sub`,
  whatever the payload claims; a credential without `sub` posts with no
  sender. A `publish` scope does not post: it sends through a relay, so a
  credential meant for posting can't publish into the relay directly, and a
  publisher's can't post.
- **Reading** history (`GET`) needs a `fetch` scope matching the broadcast path
  and the track name.
- **Redacting** (`DELETE`) needs a `redact` scope matching the broadcast path
  and the track name. The redaction carries the credential's `sub` as its
  sender. No other scope redacts, so a sender's `post` credential can't take
  anyone's record out; an app that lets senders redact their own records
  checks the record and redacts with a credential it keeps to itself.

```json
{"sub": "alice",
 "scopes": [{"actions": ["post"], "broadcast": {"exact": "room/123"}, "track": {"exact": "chat"}},
            {"actions": ["fetch"], "broadcast": {"prefix": "room/123"}}],
 "iat": 1791370000, "nbf": 1791370000, "exp": 1791370600}
```

posts into `chat` of `/room/123` as `alice`, and reads any track of
`/room/123` and the broadcasts beneath it. It posts into no other track.

A `path_auth` credential is read as the scopes it amounts to: its `sub` reads
any track at or beneath its path. Its `pub` is publishing, so it posts
nothing.

The credential is checked on every request: once it expires or its key leaves
the key set, it is refused. A missing or invalid credential answers `401` with
`WWW-Authenticate: Bearer`, and one that does not cover the broadcast or the
track `403`.

## Publishing through a relay

With `RELAY_URL`, the funnel dials the relay as a client and announces its
broadcasts on that session; the relay subscribes to them when its subscribers
do. The session needs a credential that may publish there. The funnel either
uses the `?jwt=` in `RELAY_URL`, or, with `RELAY_SIGNING_KEY` and
`RELAY_PUBLISH`, signs a fresh one for every session, granting `publish` on
`RELAY_PUBLISH` and every path beneath it, from a key the relay's key
set trusts (`qumo auth keygen`, ideally confined with `-prefix`). A credential
lasts at most an hour, so one in `RELAY_URL` admits sessions only until then;
a long-running funnel signs its own.

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
| `FUNNEL_SENDER_LIMIT` | (unset) | Records each sender may send into a track, as `RATE,BURST`: per second, and at once, e.g. `1,5`. Unset is no limit. |
| `FUNNEL_TRACK_LIMIT` | (unset) | Records a track takes from all senders together, as `RATE,BURST`, e.g. `20,40`. Unset is no limit. |
| `RELAY_URL` | (unset) | The relay to publish through, dialed as a client: `https://relay:4433` for WebTransport or `moqt://relay:4433` for native QUIC. It may carry a credential as `?jwt=`. |
| `RELAY_SIGNING_KEY` / `RELAY_PUBLISH` | (unset) | A signing key file and the path its credentials grant publishing at, e.g. `room`. Set together; the funnel signs a fresh relay credential for every session in place of `?jwt=`. |
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
