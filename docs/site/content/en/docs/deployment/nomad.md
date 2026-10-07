---
title: Nomad
description: Deploying qumo relays on Nomad using the static PEERS topology.
weight: 3
---

qumo has no runtime peer-discovery service — relays connect to a fixed,
comma-separated list of addresses configured via `PEERS`
(see [Configuration → Static peers]({{< relref "../configuration" >}}#static-peers)).
Running on Nomad is no different: point each relay's `PEERS` at a stable address for the peer(s) it should connect to.

## How it works

- Each relay is a normal Nomad task; no `service` discovery lookups happen at
  the relay's own initiative.
- To give a relay a **stable address** other relays can dial (e.g. a hub other
  edges connect to), give its Nomad job a fixed, well-known address — a static
  host port, a Consul DNS name (e.g. `role-hub.qumo-relay.service.consul:4433`
  when running Consul alongside Nomad), or a Docker network alias if relays
  share a Docker network (see the demo below).
- **Edges** set `PEERS` to their hub's address(es). **Hubs** leave it
  unset unless they too connect upward (e.g. a further regional hub).
- **Every relay that peers needs a peer identity**: the relay CA's
  certificate and a peer certificate of its own, as files in the task
  (`CA_FILE`, `PEER_CERT_FILE`, `PEER_KEY_FILE`). That includes a hub that
  dials no one, since it is dialed. A relay with `PEERS` and no identity
  refuses to start. See
  [TLS & mTLS]({{< relref "tls" >}}#relay-peers-optional) for what the
  certificate must contain.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `PEERS` | (empty) | Comma-separated relays to dial, e.g. an edge's hub(s); each host is resolved to all its addresses. Needs the three settings below. |
| `CA_FILE` | (empty) | The relay CA's certificate (PEM), a path relative to the task's working directory. |
| `PEER_CERT_FILE` / `PEER_KEY_FILE` | (empty) | This relay's own peer certificate and key, issued by that CA. One per relay: render them per allocation, never one pair for the whole job. |

`--role hub`/`--role edge` is a CLI flag on `qumo relay` — it is an
operator-facing label logged at startup for visibility only and does not
affect which addresses are dialed. See
[CLI → relay]({{< relref "../cli/relay" >}}).

## Demo job spec

`docker/nomad/qumo-cluster.nomad.hcl` runs a real single-region Nomad cluster
exercising this topology: 2 hubs, each given a stable Docker-network alias via
the docker driver's `network_aliases` config, and 2 edges with a fixed
`PEERS = "hub-0:4433,hub-1:4433"`. See
[`docker/nomad/README.md`](https://github.com/qumo-dev/qumo/blob/main/docker/nomad/README.md)
for how to run and verify it.

```hcl
# hub task
config {
  network_mode    = "qumo-net"
  network_aliases = ["hub-${NOMAD_ALLOC_INDEX}"]
  args            = ["relay", "--role", "hub"]
}

# edge task
env {
  PEERS          = "hub-0:4433,hub-1:4433"
  CA_FILE        = "certs/peer-ca.crt"
  PEER_CERT_FILE = "certs/peers/edge-asia-${NOMAD_ALLOC_INDEX}.crt"
  PEER_KEY_FILE  = "certs/peers/edge-asia-${NOMAD_ALLOC_INDEX}.key"
}
```

The hub task sets the same three peer settings with its own files, and no
`PEERS`.

For a production Nomad deployment with Consul installed, a Consul DNS name
(`<service>.service.consul`) is usually the simpler stable address to put in
`PEERS` instead of a fixed per-index alias.

## Verify

```bash
nomad service info qumo-relay
```

On an **edge**, `qumo_relay_peers_connected` (from `/metrics`) should reach
the number of addresses in its `PEERS`; on a **hub** with no
`PEERS` set, it stays `0`.
