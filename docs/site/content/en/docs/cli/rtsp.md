---
title: rtsp
description: Pull from an RTSP source (e.g. an IP camera) and republish as MoQT.
weight: 3
---

Connects to an RTSP source (e.g. an IP camera), pulls the stream via
`DESCRIBE`/`SETUP`/`PLAY`, and republishes it as MoQT — with automatic
reconnect on failure, served on its own MoQT listener or published through a
relay. Like [rtmp]({{< relref "rtmp" >}}) and
[rtsp-push]({{< relref "rtsp-push" >}}), it does not join the relay peer mesh
as a peer.

## Usage

```
qumo rtsp <rtsp-url> [broadcast-path]
```

| Argument | Default | Description |
|---|---|---|
| `<rtsp-url>` | (required) | Source to pull from — `rtsp://[user:pass@]host/path`. |
| `[broadcast-path]` | `/live/camera` | MoQT broadcast path to republish on. |

## Example

```console
$ qumo rtsp rtsp://user:pass@192.168.1.50/stream1 /live/camera
INFO ingest session started broadcast_path=/live/camera
INFO RTSP pull ingest starting source=rtsp://192.168.1.50/stream1 broadcast_path=/live/camera serve=:4433
```

Subscribers then consume `/live/camera` over MoQT on `:4433`. If the source
is unreachable or drops, it logs the failure and retries with a doubling
backoff rather than exiting:

```console
WARN RTSP pull disconnected, reconnecting error="dial: rtsp: dial 192.168.1.50:554: ..." backoff=2s
WARN RTSP pull disconnected, reconnecting error="dial: rtsp: dial 192.168.1.50:554: ..." backoff=4s
```

## Publishing through a relay

With `RELAY_URL` set, the command publishes what it pulls through that relay,
as a client, and the relay serves viewers — and checks their credentials. It
then opens no MoQT listener of its own unless `RTSP_SERVE_ADDR` is set: that
listener checks no subscriber, so with auth on, viewers go through the relay.

```console
$ RELAY_URL=moqt://relay:4433 RELAY_SIGNING_KEY=ingest-key.jwk RELAY_PUBLISH=/live/ qumo rtsp rtsp://camera/stream /live/camera
```

## Configuration

| Variable | Default | Description |
|---|---|---|
| `RTSP_SERVE_ADDR` | `:4433`; none with `RELAY_URL` | MoQT listen address for subscribers to dial directly. |
| `RELAY_URL` | (unset) | The relay to publish through, dialed as a client (`https://…` or `moqt://…`); it may carry a credential as `?jwt=`. |
| `RELAY_SIGNING_KEY`, `RELAY_PUBLISH` | (unset) | Set together: a signing key (`qumo auth keygen`) the server signs a fresh relay credential with on every dial and renews on the session, granting publishing beneath the directory `RELAY_PUBLISH`. A broadcast path outside it stops the command at startup. |
| `RELAY_CA_FILE` / `RELAY_TLS_INSECURE` | (unset) / `false` | Trust this PEM as the relay's root, or skip verification for a self-signed dev relay. |
| `CERT_FILE` / `KEY_FILE` | `certs/server.crt` / `certs/server.key` | TLS certificate and key. |
| `CORS_ALLOWED_ORIGINS` | (unset) | Comma-separated WebTransport origins (default: same-origin only; `*` allows any). |

## See also

- [rtsp-push]({{< relref "rtsp-push" >}}) — the **push** direction, where the camera dials qumo instead.
- [rtmp]({{< relref "rtmp" >}}) — the RTMP equivalent of this command.
- [Deployment → Docker]({{< relref "../deployment/docker" >}}) — running it as a container.
- [Deployment → TLS & mTLS]({{< relref "../deployment/tls" >}}) — generating the certificate it needs.
