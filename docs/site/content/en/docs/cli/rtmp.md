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

## Publishing through a relay

With `RELAY_URL` set, the server publishes what it ingests through that relay,
as a client, and the relay serves viewers — and checks their credentials. It
then opens no MoQT listener of its own unless `RTMP_SERVE_ADDR` is set: that
listener checks no subscriber, so with auth on, viewers go through the relay.

```console
$ RELAY_URL=moqt://relay:4433 RELAY_SIGNING_KEY=ingest-key.jwk RELAY_PUBLISH=/live/ qumo rtmp
```

## Configuration

| Variable | Default | Description |
|---|---|---|
| `RTMP_INGEST_ADDR` | `:1935` | RTMP listen address. |
| `RTMP_SERVE_ADDR` | `:4433`; none with `RELAY_URL` | MoQT listen address for subscribers to dial directly. |
| `QUMO_AUTH_KEYS` | (unset) | The key set a publisher's credential is verified against, as the relay reads it (a file or URL). Unset accepts every publish. |
| `QUMO_AUTH_KEYS_CACHE` / `QUMO_RELAY_TOKEN` | (unset) | A file a downloaded key set is kept in, and the bearer token sent to a key-set URL. |
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
