---
title: funnel
description: Funnel many HTTP senders into one MoQT track, recording each record before it is sent.
weight: 5
---

Starts a standalone server that funnels many HTTP senders into one MoQT
track. Contributors POST records into a track; each record is committed to a [qumo-ledger](https://github.com/okdaichi/qumo-ledger)
track and then sent to the track's MoQT subscribers. Like [rtmp]({{< relref "rtmp" >}}),
this is a self-contained origin and does not join the relay peer mesh.

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
$ LEDGER_URI=file:///var/lib/qumo QUMO_AUTH_KEYS=keys.json qumo funnel
	Ingest  : :8090
	Serve   : :4433
	Ledger  : file:///var/lib/qumo
	Auth    : keys.json
```

Without `LEDGER_URI`, records are kept in memory and are lost when the
process exits. Without `QUMO_AUTH_KEYS`, every contributor is accepted.

A contributor announces itself in a track, then records to the contribution the
announce returns:

```console
$ curl -i -X POST localhost:8090/announce \
    -H "Authorization: Bearer $ALICE" \
    -d '{"broadcast_path": "/room/123", "track_name": "chat", "name": "alice"}'
HTTP/1.1 201 Created
Location: contributions/JQ4ZJ2XHS7NQ7E3KVM6UXWIZBA
{"id":"JQ4ZJ2XHS7NQ7E3KVM6UXWIZBA","track":"room/123/chat","created":true}

$ curl -X POST localhost:8090/contributions/JQ4ZJ2XHS7NQ7E3KVM6UXWIZBA/records \
    -H "Authorization: Bearer $ALICE" \
    -d '{"text": "hello"}'
{"group":"e000001-g00000000","wallclock":1791370000000000000}
```

Subscribers consume the track `chat` of the broadcast `/room/123` over MoQT on
`:4433`. Any number of contributors record into the same track, and a
subscriber receives all of them on one subscription.

## Requests

| Endpoint | Body | Result |
|---|---|---|
| `POST /announce` | `{"broadcast_path", "track_name", "name"}` | Creates the track when it does not exist and starts a contribution: `201` with `Location: contributions/{id}`. |
| `POST /contributions/{id}/records` | The record's payload, any JSON value | `201` once the record is committed. |
| `DELETE /contributions/{id}` | — | Ends the contribution: `204`. |

`track_name` must not contain a slash, and `name` is one path segment.

A contributor has one contribution per track. Announcing again under the same
name ends the previous one, and so does recording nothing for five minutes.
Requests to an ended contribution answer `410`, and to an unknown one `404`.

## Credentials

With a key set (`QUMO_AUTH_KEYS`), every request carries a qumo credential as
`Authorization: Bearer`, the same kind the relay admits sessions with and
verified against the same key set. Its publish grant must cover the broadcast
path joined with the contributor's name: `/room/123/alice` for alice in
`/room/123`, or a grant for `/room/123` itself. So a credential that lets alice
publish at `/room/123/alice` is what lets her record as `alice`.

The credential is checked on every request, not only the announce: once it
expires or its key leaves the key set, the contribution's records are refused.
A missing or invalid credential answers `401`, and one that does not cover the
contributor `403`.

## What a subscriber receives

Each record is one group holding one frame, the JSON object:

```json
{"name": "alice", "payload": {"text": "hello"}}
```

A record is sent only after it is committed, in commit order. A group's
sequence is one more than the sequence of the ledger group that stores the
record. A new subscriber starts at the track's latest record.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `FUNNEL_ADDR` | `:8090` | HTTP listen address for `announce` and `record`. |
| `FUNNEL_SERVE_ADDR` | `:4433` | MoQT listen address. |
| `LEDGER_URI` | (unset) | Where records are stored. The scheme selects the backend: `file:///var/lib/qumo` is a directory (`file:ledger` for a relative one); `postgres://user@host:26257/qumo` is a table (`ledger_objects`, or `?table=`) in PostgreSQL or CockroachDB; `s3://bucket/prefix?region=…` is a bucket of S3, or of an S3-compatible service with `&endpoint=http://host:9000`, with credentials from `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`; unset or empty is memory. A bare path or any other scheme is an error. |
| `CERT_FILE` / `KEY_FILE` | `certs/server.crt` / `certs/server.key` | TLS certificate and key for MoQT. |
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
- **A quiet track answers late.** A subscription to a track with no record yet
  is answered when the first record arrives.
- **Recorded tracks are not served over HLS.** Records carry no duration, which
  the [hls]({{< relref "hls" >}}) renderers need.

## See also

- [rtmp]({{< relref "rtmp" >}}) — ingest media over RTMP.
- [hls]({{< relref "hls" >}}) — the other command built on qumo-ledger.
