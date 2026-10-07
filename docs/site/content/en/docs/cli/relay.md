---
title: relay
description: Start the MoQT relay server.
weight: 1
---

The core `qumo` command: accepts publishers and subscribers over
QUIC/WebTransport, and meshes with peer relays for content discovery.

## Usage

```
qumo relay [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--role <hub\|edge>` | flat / single-node | Node topology role. Everything else is configured via environment variables. |

## Example

It needs a TLS certificate to bind at all — `CERT_FILE`/`KEY_FILE`, default
`certs/server.crt`/`certs/server.key` — or it fails at startup with a
"failed to setup TLS" error.

```console
$ RELAY_ADDR=127.0.0.1:4443 RELAY_NAME=relay-1 qumo relay
INFO relay: UDP receive buffer size="default 262144 (256 KB)"
	Host    : 127.0.0.1:4443
	Node ID : relay-1
	/       : WebTransport endpoint
	/health : health probe
	/metrics: Prometheus metrics
	Resolver: local (qumo-relay) (interval: 15s)
```

Add `--role hub` or `--role edge` to give the node a topology role instead of
running flat. Once it's up:

```console
$ curl http://127.0.0.1:4443/health
{"live":true,"ready":true,"timestamp":"2026-08-09T11:28:33.9277972+09:00","uptime":"3.1378795s"}
```

## Configuration

Everything other than `--role` is an environment variable: bind address, TLS
certificates, peer discovery, capacity tuning, and session auth. Unlike
the ingest commands, the relay's surface is large enough to have its own
page — see [Configuration]({{< relref "../configuration" >}}) for the full
reference.

## See also

- [Configuration]({{< relref "../configuration" >}}) — the full environment variable reference.
- [Deployment → Peer topology]({{< relref "../deployment/peer-topology" >}}) — how `--role` fits into peer discovery.
- [Deployment → TLS & mTLS]({{< relref "../deployment/tls" >}}) — generating the certificate it needs.
- [Deployment → Docker]({{< relref "../deployment/docker" >}}) — running it as a container.
- [Observability]({{< relref "../observability" >}}) — `/health`, `/metrics`, and `qumo doctor`.
