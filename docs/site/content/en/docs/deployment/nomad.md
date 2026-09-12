---
title: Nomad
description: Deploying qumo relays on Nomad using the static UPSTREAM_ADDR / PEERS topology.
weight: 3
---

qumo has no runtime peer-discovery service — relays connect to a fixed,
comma-separated list of addresses configured via `PEERS` and `UPSTREAM_ADDR`
(see [Configuration → Static peers]({{< relref "../configuration" >}}#static-peers)).
Running on Nomad is no different: point each relay's `UPSTREAM_ADDR` (and/or
`PEERS`) at a stable address for the peer(s) it should connect to.

## How it works

- Each relay is a normal Nomad task; no `service` discovery lookups happen at
  the relay's own initiative.
- To give a relay a **stable address** other relays can dial (e.g. a hub other
  edges connect to), give its Nomad job a fixed, well-known address — a static
  host port, a Consul DNS name (e.g. `role-hub.qumo-relay.service.consul:4433`
  when running Consul alongside Nomad), or a Docker network alias if relays
  share a Docker network (see the demo below).
- **Edges** set `UPSTREAM_ADDR` to their hub's address(es). **Hubs** leave it
  unset unless they too connect upward (e.g. a further regional hub).

## Configuration

| Variable | Default | Description |
|---|---|---|
| `UPSTREAM_ADDR` | (empty) | Comma-separated upstream relay address(es) to dial, e.g. an edge's hub(s). |
| `PEERS` | (empty) | Comma-separated static peer addresses (symmetric peering, not upstream-specific). |

`--role hub`/`--role edge` is a CLI flag on `qumo relay` — it is an
operator-facing label logged at startup for visibility only and does not
affect which addresses are dialed. See
[CLI → relay]({{< relref "../cli/relay" >}}).

## Demo job spec

`docker/nomad/qumo-cluster.nomad.hcl` runs a real single-region Nomad cluster
exercising this topology: 2 hubs, each given a stable Docker-network alias via
the docker driver's `network_aliases` config, and 2 edges with a fixed
`UPSTREAM_ADDR = "hub-0:4433,hub-1:4433"`. See
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
  UPSTREAM_ADDR = "hub-0:4433,hub-1:4433"
}
```

For a production Nomad deployment with Consul installed, a Consul DNS name
(`<service>.service.consul`) is usually the simpler stable address to put in
`UPSTREAM_ADDR` instead of a fixed per-index alias.

## Verify

```bash
nomad service info qumo-relay
```

On an **edge**, `qumo_relay_peers_connected` (from `/metrics`) should reach
the number of addresses in its `UPSTREAM_ADDR`; on a **hub** with no
`UPSTREAM_ADDR`/`PEERS` set, it stays `0`.
