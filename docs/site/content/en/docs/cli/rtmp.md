---
title: rtmp
description: Start the RTMP ingest server, bridging published streams to MoQT.
weight: 2
---

Starts an RTMP ingest server that bridges published streams to MoQT, served
on its own MoQT listener or published through a relay. Like
[rtsp]({{< relref "rtsp" >}}) and [rtsp-push]({{< relref "rtsp-push" >}}), it
does not join the relay peer mesh as a peer.

## Usage

```
qumo rtmp
```

Takes no flags or arguments — configured entirely through environment
variables.

## Example

```console
$ qumo rtmp
	Ingest  : :1935
	Serve   : :4433
```

Publishers then push to `rtmp://<host>:1935/<app>/<stream key>`, and
subscribers consume `/<app>/<stream key>` over MoQT on `:4433`; a `?query` on
the stream key is not part of the path. Override either address to taste:

```console
$ RTMP_INGEST_ADDR=:1936 RTMP_SERVE_ADDR=127.0.0.1:4456 qumo rtmp
	Ingest  : :1936
	Serve   : 127.0.0.1:4456
```

## Publisher auth

With `QUMO_AUTH_KEYS`, every push presents a credential: a token
(`qumo auth token`) that may `publish` the broadcast, as `?jwt=` on the stream key (OBS: Stream Key `cam?jwt=TOKEN`) or on the server URL
(`rtmp://host:1935/live?jwt=TOKEN`).
A missing, invalid or paused-key credential is refused (`NetStream.Publish.Denied`), and a push
is cut when its token expires, at most an hour on: the encoder reconnects with
a fresh one.

```console
$ QUMO_AUTH_KEYS=keys.json qumo rtmp
# OBS: Server rtmp://host:1935/live, Stream Key cam?jwt=$(qumo auth token -root /live/ -publish /cam)
```

## Opting streams into HLS and ABR

A pushed stream can't announce an opt-in for [`qumo hls`]({{< relref "hls" >}})
or [`qumo abr`]({{< relref "abr" >}}) itself, as an MoQ publisher does, so the
server announces it on the publisher's behalf: `/.abr/live/cam` beside
`/live/cam`, a redirect to the broadcast. `INGEST_OPT_IN` lists the egresses
it offers (`hls`, `abr`, comma-separated; none by default).

With auth off, every stream is opted into each of them. With auth on, a
stream is opted into one only when a token it presents may publish the
opt-in: a second `?jwt=` on the stream key, signed for the opt-in's path
(`-root /.abr/live/ -publish /cam`). The relay's key set decides, as for an
MoQ publisher: a tenant's key signs it only while its entry lists the opt-in
(`"optIns": ["abr"]`, see [abr → Paying for the opt-in]({{< relref "abr#paying-for-the-opt-in" >}})).
A refused opt-in token leaves the stream on air without the opt-in, and the
opt-in is withdrawn when its token expires. A push to an opt-in's path itself
(`/.abr/live/cam`) is refused: the server keeps those paths for its opt-ins.

```console
$ QUMO_AUTH_KEYS=keys.json INGEST_OPT_IN=abr qumo rtmp
# OBS: Stream Key cam?jwt=$(qumo auth token -root /live/ -publish /cam)&jwt=$(qumo auth token -root /.abr/live/ -publish /cam)
```

Through a relay with `RELAY_SIGNING_KEY`, the server also presents a ticket
for each egress in `INGEST_OPT_IN` (publishing beneath `/.abr/live/` for
`RELAY_PUBLISH=/live/`), so the relay takes the opt-ins: the key must hold the
opt-ins in the relay's key set, as a key for the whole namespace (`-prefix /`)
does.

## Publishing through a relay

With `RELAY_URL` set, the server publishes what it ingests through that relay,
as a client, and the relay serves viewers — and checks their credentials. It
then opens no MoQT listener of its own unless `RTMP_SERVE_ADDR` is set: that
listener checks no subscriber, so with auth on, viewers go through the relay.

```console
$ RELAY_URL=moqt://relay:4433 RELAY_SIGNING_KEY=ingest-key.jwk RELAY_PUBLISH=/live/ qumo rtmp
```

## Usage

With `QUMO_USAGE_URL` (and `QUMO_AUTH_KEYS`), the server reports each pushed
stream as a relay reports a publishing session ([usage reports]({{< relref "../configuration#usage-reports" >}})),
under the key of the token that granted the push: a `session_open`, a `usage`
record every 30 s and a `session_close`, carrying the media it took in
(`ingest.media_bytes`, the payload of every frame) and how long it kept its
broadcast opted into each egress (`gateway.opt_in_seconds.abr`,
`gateway.opt_in_seconds.hls`). The relay sees the server's own session, under
the server's key, so the stream is billed here, to its publisher.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `RTMP_INGEST_ADDR` | `:1935` | RTMP listen address. |
| `RTMP_SERVE_ADDR` | `:4433`; none with `RELAY_URL` | MoQT listen address for subscribers to dial directly. |
| `QUMO_AUTH_KEYS` | (unset) | The key set a publisher's credential is verified against, as the relay reads it (a file or URL). Unset accepts every publish. |
| `QUMO_AUTH_KEYS_CACHE` / `QUMO_RELAY_TOKEN` | (unset) | A file a downloaded key set is kept in, and the bearer token sent to a key-set URL and to `QUMO_USAGE_URL`. |
| `QUMO_USAGE_URL` | (unset) | Where each stream's usage is reported, under the key of the publisher's token ([usage](#usage)). Needs `QUMO_AUTH_KEYS`. |
| `INGEST_OPT_IN` | (unset) | The egresses (`hls`, `abr`) a stream is opted into on its publisher's behalf, comma-separated: with auth on, those a second `?jwt=` may publish the opt-in of. |
| `RELAY_URL` | (unset) | The relay to publish through, dialed as a client (`https://…` or `moqt://…`); it may carry a credential as `?jwt=`. |
| `RELAY_SIGNING_KEY`, `RELAY_PUBLISH` | (unset) | Set together: a signing key (`qumo auth keygen`) the server signs a fresh relay credential with on every dial and renews on the session, granting publishing beneath the directory `RELAY_PUBLISH`. A push outside it is refused. |
| `RELAY_CA_FILE` / `RELAY_TLS_INSECURE` | (unset) / `false` | Trust this PEM as the relay's root, or skip verification for a self-signed dev relay. |
| `CERT_FILE` / `KEY_FILE` | `certs/server.crt` / `certs/server.key` | TLS certificate and key. |
| `CORS_ALLOWED_ORIGINS` | (unset) | Comma-separated WebTransport origins (default: same-origin only; `*` allows any). |

## See also

- [rtsp]({{< relref "rtsp" >}}) — pull an RTSP source into MoQT.
- [rtsp-push]({{< relref "rtsp-push" >}}) — the RTSP equivalent of this command.
- [Deployment → Docker]({{< relref "../deployment/docker" >}}) — running it as a container.
- [Deployment → TLS & mTLS]({{< relref "../deployment/tls" >}}) — generating the certificate it needs.
