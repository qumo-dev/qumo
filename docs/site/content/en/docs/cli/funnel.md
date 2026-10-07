---
title: funnel
description: Funnel many HTTP senders into one MoQT track, recording each record before it is sent.
weight: 5
---

Funnels many HTTP senders into one MoQT track. Contributors POST records into a
track; each record is committed to a [qumo-ledger](https://github.com/okdaichi/qumo-ledger)
track and then published as MoQT. The funnel publishes through a relay it
dials as a client, so subscribers reach its tracks on the relay they already
use; it can also serve them on a listener of its own. It does not join the
relay mesh as a peer.

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
process exits. Without `QUMO_AUTH_KEYS`, every contributor is accepted.
Without `RELAY_URL`, the funnel serves subscribers itself on `:4433`.

A contributor announces a track, then records to the contribution the
announce returns:

```console
$ curl -i -X POST localhost:8090/announce \
    -H "Authorization: Bearer $CREDENTIAL" \
    -d '{"broadcast_path": "/room/123", "track_name": "chat"}'
HTTP/1.1 201 Created
Location: contributions/JQ4ZJ2XHS7NQ7E3KVM6UXWIZBA
{"id":"JQ4ZJ2XHS7NQ7E3KVM6UXWIZBA","track":"room/123/chat"}

$ curl -X POST localhost:8090/contributions/JQ4ZJ2XHS7NQ7E3KVM6UXWIZBA/records \
    -H "Authorization: Bearer $CREDENTIAL" \
    -H "Idempotency-Key: 7f9c2b" \
    -d '{"user": "alice", "text": "hello"}'
{"group":"e000001-g00000000","wallclock":1791370000000000000}
```

Subscribers consume the track `chat` of the broadcast `/room/123`. Any number
of contributions record into the same track, and a subscriber receives all of
them on one subscription.

## Requests

| Endpoint | Body | Result |
|---|---|---|
| `POST /announce` | `{"broadcast_path", "track_name"}` | Creates the track when it does not exist and starts a contribution: `201` with `Location: contributions/{id}`. |
| `POST /contributions/{id}/records` | The record's payload, one JSON value in UTF-8 | `201` once the record is committed. |
| `DELETE /contributions/{id}` | — | Ends the contribution: `204`. |

`track_name` must not contain a slash. The payload is stored and sent as it
was posted: whatever identifies its author belongs in it.

A record sent with an `Idempotency-Key` header is stored once per
contribution: a retry with the same key is answered as the first was and
stores nothing. A contribution remembers its 256 most recent keys.

A contribution ends when it is deleted or has recorded nothing for five
minutes. Requests to an ended contribution answer `410`, and to an unknown one
`404`.

## Credentials

With a key set (`QUMO_AUTH_KEYS`), every request carries a qumo credential as
`Authorization: Bearer`, the same kind the relay admits sessions with and
verified against the same key set. Its publish grant must cover the broadcast
path: a credential for `/room/123`, or for `/room`, may record into
`/room/123`.

The credential is checked on every request, not only the announce: once it
expires or its key leaves the key set, the contribution's records are refused.
A missing or invalid credential answers `401` with `WWW-Authenticate: Bearer`,
and one that does not cover the broadcast `403`.

## Publishing through a relay

With `RELAY_URL`, the funnel dials the relay as a client and announces its
broadcasts on that session; the relay subscribes to them when its subscribers
do. The session needs a credential that may publish there. The funnel either
uses the `?jwt=` in `RELAY_URL`, or, with `RELAY_SIGNING_KEY` and
`RELAY_PUBLISH`, signs a fresh one for every session from a key the relay's key
set trusts (`qumo auth keygen`, ideally confined with `-prefix`). Credentials
last at most an hour and the relay ends a session when its credential
expires, so a long-running funnel signs its own.

When a session ends, the funnel dials again after two seconds and announces
its broadcasts anew.

## What a subscriber receives

Each record is one group holding one frame, the payload as it was posted. A
record is sent only after it is committed, in commit order. A group's
sequence is one more than the sequence of the ledger group that stores the
record. A new subscriber starts at the track's latest record.

A subscriber reaches a track once a contributor has announced it. A
subscription to any other track name of the broadcast is refused as not found.

At startup the funnel lists its store once and publishes every track it
recorded before, with its latest record, so subscribers reach them across a
restart before anyone announces again.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `FUNNEL_ADDR` | `:8090` | HTTP listen address for `announce` and `record`. |
| `RELAY_URL` | (unset) | The relay to publish through, dialed as a client: `https://relay:4433` for WebTransport or `moqt://relay:4433` for native QUIC. It may carry a credential as `?jwt=`. |
| `RELAY_SIGNING_KEY` / `RELAY_PUBLISH` | (unset) | A signing key file and the path its credentials grant publishing at, e.g. `room`. Set together; the funnel signs a fresh relay credential for every session in place of `?jwt=`. |
| `RELAY_CA_FILE` | (unset) | PEM certificate to trust as the relay's root, instead of the system roots. |
| `RELAY_TLS_INSECURE` | `false` | Skip relay TLS verification, for a self-signed dev relay. |
| `FUNNEL_SERVE_ADDR` | `:4433` without `RELAY_URL`, none with it | MoQT listen address for subscribers that dial the funnel directly. |
| `LEDGER_URI` | (unset) | Where records are stored. The scheme selects the backend: `file:///var/lib/qumo` is a directory (`file:ledger` for a relative one); `postgres://user@host:26257/qumo` is a table (`ledger_objects`, or `?table=`) in PostgreSQL or CockroachDB; `s3://bucket/prefix?region=…` is a bucket of S3, or of an S3-compatible service with `&endpoint=http://host:9000`, with credentials from `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`; unset or empty is memory. A bare path or any other scheme is an error. |
| `CERT_FILE` / `KEY_FILE` | `certs/server.crt` / `certs/server.key` | TLS certificate and key for the funnel's own MoQT listener. |
| `CORS_ALLOWED_ORIGINS` | (unset) | Comma-separated origins allowed to call the HTTP endpoints from a browser and to open WebTransport (default: same-origin only; `*` allows any). |
| `QUMO_AUTH_KEYS` | (unset) | The key set contributors' credentials are verified against, as the relay reads it: a file, or an https URL. Unset accepts every contributor. |
| `QUMO_AUTH_KEYS_CACHE` | (unset) | A file a downloaded key set is kept in between runs. |
| `QUMO_RELAY_TOKEN` | (unset) | Bearer token sent to a key-set URL. |

## Limits

- **Plain HTTP.** The HTTP listener does not terminate TLS; put it behind
  something that does, since credentials travel in its requests.
- **Memory is not bounded.** With no `LEDGER_URI`, every record stays in
  memory until the process exits. Use it for development.
- **One process per track.** Records of a track are ordered within one process;
  two processes recording into the same track are not coordinated.
- **Contributions do not survive a restart.** Contributors announce again;
  the IDs of a previous run answer `404`, and their idempotency keys are
  forgotten.
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
