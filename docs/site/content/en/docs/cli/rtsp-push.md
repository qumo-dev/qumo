---
title: rtsp-push
description: Start the RTSP push ingest server (ANNOUNCE/RECORD), bridging published streams to MoQT.
weight: 4
---

Starts an RTSP **push** ingest server (`ANNOUNCE`/`RECORD`) that bridges
published streams to MoQT, served on its own MoQT listener or published through
a relay. Like [rtmp]({{< relref "rtmp" >}}) and [rtsp]({{< relref "rtsp" >}}),
it does not join the relay peer mesh as a peer.

## Usage

```
qumo rtsp-push
```

Takes no flags or arguments — configured entirely through environment
variables.

## Example

```console
$ qumo rtsp-push
	Ingest  : :8554
	Serve   : :4433
```

Encoders then `ANNOUNCE`/`RECORD` to `rtsp://<host>:8554/<path>`, and
subscribers consume the same path over MoQT on `:4433`. Override either
address to taste:

```console
$ RTSP_INGEST_ADDR=:8555 RTSP_SERVE_ADDR=127.0.0.1:4457 qumo rtsp-push
	Ingest  : :8555
	Serve   : 127.0.0.1:4457
```

## Publisher auth

With `QUMO_AUTH_KEYS`, every push presents a credential: a token
(`qumo auth token`) that may `publish` the broadcast, as `?jwt=` on the URL it pushes to.
A missing, invalid or paused-key credential is refused (`401 Unauthorized` to the `ANNOUNCE`), and a push
is cut when its token expires, at most an hour on: the encoder reconnects with
a fresh one.

```console
$ QUMO_AUTH_KEYS=keys.json qumo rtsp-push
$ ffmpeg -re -i in.mp4 -c copy -f rtsp "rtsp://host:8554/live/cam?jwt=$(qumo auth token -root /live/ -publish /cam)"
```

## Opting streams into HLS and ABR

A pushed stream can't announce an opt-in for [`qumo hls`]({{< relref "hls" >}})
or [`qumo abr`]({{< relref "abr" >}}) itself, as an MoQ publisher does, so the
server announces it on the publisher's behalf: `/.abr/live/cam` beside
`/live/cam`, a redirect to the broadcast. `INGEST_OPT_IN` lists the egresses
it offers (`hls`, `abr`, comma-separated; none by default).

With auth off, every stream is opted into each of them. With auth on, a
stream is opted into one only when a token it presents may publish the
opt-in: a second `?jwt=` on the URL, signed for the opt-in's path
(`-root /.abr/live/ -publish /cam`). The relay's key set decides, as for an
MoQ publisher: a tenant's key signs it only while its entry lists the opt-in
(`"optIns": ["abr"]`, see [abr → Paying for the opt-in]({{< relref "abr#paying-for-the-opt-in" >}})).
A refused opt-in token leaves the stream on air without the opt-in, and the
opt-in is withdrawn when its token expires. A push to an opt-in's path itself
(`/.abr/live/cam`) is refused: the server keeps those paths for its opt-ins.

```console
$ QUMO_AUTH_KEYS=keys.json INGEST_OPT_IN=abr qumo rtsp-push
$ ffmpeg -re -i in.mp4 -c copy -f rtsp "rtsp://host:8554/live/cam?jwt=$(qumo auth token -root /live/ -publish /cam)&jwt=$(qumo auth token -root /.abr/live/ -publish /cam)"
```

Through a relay with `RELAY_SIGNING_KEY`, the server also presents a ticket
for each egress in `INGEST_OPT_IN` (publishing beneath `/.abr/live/` for
`RELAY_PUBLISH=/live/`), so the relay takes the opt-ins: the key must hold the
opt-ins in the relay's key set, as a key for the whole namespace (`-prefix /`)
does.

## Publishing through a relay

With `RELAY_URL` set, the server publishes what it ingests through that relay,
as a client, and the relay serves viewers — and checks their credentials. It
then opens no MoQT listener of its own unless `RTSP_SERVE_ADDR` is set: that
listener checks no subscriber, so with auth on, viewers go through the relay.

```console
$ RELAY_URL=moqt://relay:4433 RELAY_SIGNING_KEY=ingest-key.jwk RELAY_PUBLISH=/live/ qumo rtsp-push
```

## Configuration

| Variable | Default | Description |
|---|---|---|
| `RTSP_INGEST_ADDR` | `:8554` | RTSP listen address. |
| `RTSP_SERVE_ADDR` | `:4433`; none with `RELAY_URL` | MoQT listen address for subscribers to dial directly. |
| `QUMO_AUTH_KEYS` | (unset) | The key set a publisher's credential is verified against, as the relay reads it (a file or URL). Unset accepts every publish. |
| `QUMO_AUTH_KEYS_CACHE` / `QUMO_RELAY_TOKEN` | (unset) | A file a downloaded key set is kept in, and the bearer token sent to a key-set URL. |
| `INGEST_OPT_IN` | (unset) | The egresses (`hls`, `abr`) a stream is opted into on its publisher's behalf, comma-separated: with auth on, those a second `?jwt=` may publish the opt-in of. |
| `RELAY_URL` | (unset) | The relay to publish through, dialed as a client (`https://…` or `moqt://…`); it may carry a credential as `?jwt=`. |
| `RELAY_SIGNING_KEY`, `RELAY_PUBLISH` | (unset) | Set together: a signing key (`qumo auth keygen`) the server signs a fresh relay credential with on every dial and renews on the session, granting publishing beneath the directory `RELAY_PUBLISH`. A push outside it is refused. |
| `RELAY_CA_FILE` / `RELAY_TLS_INSECURE` | (unset) / `false` | Trust this PEM as the relay's root, or skip verification for a self-signed dev relay. |
| `CERT_FILE` / `KEY_FILE` | `certs/server.crt` / `certs/server.key` | TLS certificate and key. |
| `CORS_ALLOWED_ORIGINS` | (unset) | Comma-separated WebTransport origins (default: same-origin only; `*` allows any). |

## See also

- [rtsp]({{< relref "rtsp" >}}) — the **pull** direction, where qumo dials the camera instead.
- [rtmp]({{< relref "rtmp" >}}) — the RTMP equivalent of this command.
- [Deployment → Docker]({{< relref "../deployment/docker" >}}) — running it as a container.
- [Deployment → TLS & mTLS]({{< relref "../deployment/tls" >}}) — generating the certificate it needs.
