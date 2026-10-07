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
$ LEDGER_URI=file:///var/lib/qumo qumo funnel
	Ingest  : :8090
	Serve   : :4433
	Ledger  : /var/lib/qumo
```

Without `LEDGER_URI`, records are kept in memory and are lost when the
process exits.

A contributor announces the track, then records into it:

```console
$ curl -X POST localhost:8090/announce \
    -d '{"broadcast_path": "/room/123", "track_name": "chat", "name": "alice"}'
{"track":"room/123/chat","created":true}

$ curl -X POST localhost:8090/record \
    -d '{"broadcast_path": "/room/123", "track_name": "chat", "name": "alice", "payload": {"text": "hello"}}'
{"track":"room/123/chat","group":"e000001-g00000000","wallclock":1791370000000000000}
```

Subscribers consume the track `chat` of the broadcast `/room/123` over MoQT on
`:4433`. Any number of contributors record into the same track, and a
subscriber receives all of them on one subscription.

## Requests

Both requests are a POST with a JSON body.

| Field | Description |
|---|---|
| `broadcast_path` | The broadcast the track belongs to. |
| `track_name` | The track's name. It must not contain a slash. |
| `name` | Who is contributing. |
| `payload` | The record's content, any JSON value. `record` only. |

| Endpoint | Result |
|---|---|
| `POST /announce` | Establishes the track, creating it when it does not exist: `201` when created, `200` when it already existed. |
| `POST /record` | Appends the payload to an announced track and answers `201` once it is committed. `404` for a track nobody announced. |

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
| `LEDGER_URI` | (unset) | Where records are stored. The scheme selects the backend: `file:///var/lib/qumo` is a directory (`file:ledger` for a relative one), and unset or empty is memory. A bare path or any other scheme is an error. |
| `CERT_FILE` / `KEY_FILE` | `certs/server.crt` / `certs/server.key` | TLS certificate and key for MoQT. |
| `CORS_ALLOWED_ORIGINS` | (unset) | Comma-separated origins allowed to POST from a browser and to open WebTransport (default: same-origin only; `*` allows any). |

## Limits

- **No authentication.** The HTTP listener is plain HTTP and checks no
  credentials, so anyone who can reach it can record under any name. Run it
  behind a service that authenticates contributors.
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
