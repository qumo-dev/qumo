# Nomad UPSTREAM_ADDR simulation

A real single-region Nomad cluster that exercises the static-`UPSTREAM_ADDR`
edge→hub topology on real Nomad-launched containers, complementing the plain
Docker Compose version in `docker-compose.static.yml`.

## What this verifies (and what it doesn't)

| Path | Mechanism | Covered here |
|---|---|---|
| Edge → local hubs (within a region) | static `UPSTREAM_ADDR` (fixed hub aliases) | ✅ **yes** |
| Hub → remote hubs (cross-region) | none — no dynamic cross-cluster discovery exists | ❌ no |

A **single Nomad cluster models a single region.** Each hub gets a stable
`network_aliases` name (`hub-0`, `hub-1`, ...) on the shared `qumo-net` Docker
network via the docker driver's `network_aliases` config, resolvable by other
containers on that network through Docker's embedded DNS. Edges are given a
fixed `UPSTREAM_ADDR = "hub-0:4433,hub-1:4433"` matching the hubs group's
`count`. Hubs get no `UPSTREAM_ADDR`, so within one cluster only edge→hub
connections form.

There is no dynamic peer discovery (peer routing uses static
`PEERS` and `UPSTREAM_ADDR`; see internal/relay/server.go `ConnectPeers`),
so this sim only demonstrates a fixed topology, not runtime discovery
of scaled-up/down hubs.

> This is a manual simulation. There are **no automated integration tests** wired
> to it by design.

## Run

```bash
# 1. Build the relay image into the host Docker (Nomad's docker driver runs it).
docker build -f docker/Dockerfile -t qumo:local .

# 2. Bring up Nomad + submit the qumo-cluster job (2 hubs + 2 edges, region=asia).
docker compose -f docker/docker-compose.nomad.yml up -d

# 3. Nomad UI / API.
open http://localhost:4646/ui/jobs       # or just curl below
```

## Verify

All `nomad` commands below assume `export NOMAD_ADDR=http://localhost:4646`.

**1. The four relays are running:**

```bash
nomad job status qumo-cluster            # 2 hubs + 2 edges "running"
nomad service info qumo-relay            # 4 instances; tags role=hub|edge, region=asia
```

**2. Edges actually connected to both hubs.** Each edge should reach
`qumo_relay_peers_connected = 2`; hubs stay at `0` (no `UPSTREAM_ADDR` set on
hubs):

```bash
# pick an edge allocation id from `nomad job status qumo-cluster`
EDGE=<edge-alloc-id>
nomad alloc exec "$EDGE" wget -qO- http://localhost:4433/metrics \
  | grep -E 'qumo_relay_peers_connected|qumo_relay_peer_dial_attempts'
```

Expected on an **edge**: `qumo_relay_peers_connected 2` and
`qumo_relay_peer_dial_attempts{...,result="ok"}` ≥ 2.
Expected on a **hub**: `qumo_relay_peers_connected 0`.

## Tear down

```bash
nomad job stop -purge qumo-cluster                       # stop relay containers
docker compose -f docker/docker-compose.nomad.yml down   # stop Nomad + network
```

## Troubleshooting

This sim was authored without a Docker host to validate against; the Nomad↔Docker
networking is the most likely thing to need a small tweak. Common issues:

- **`Failed to find image qumo:local`** — run the `docker build ... -t qumo:local`
  step first; Nomad's docker driver pulls from the *host* Docker images.
- **Edges show `peers_connected 0`** — the edge can't resolve/reach `hub-0`/`hub-1`.
  Confirm both hub tasks are `network_mode = "qumo-net"` with `network_aliases`
  set (`nomad alloc status <hub-alloc-id>` shows the rendered container config),
  and that the agent's `network_interface = "eth0"` is the qumo-net interface.
- **`/var/run/docker.sock` not found / permission denied** — on Docker Desktop
  (macOS/Windows) ensure the socket is shared; the `nomad` service mounts it and
  runs `privileged: true` to drive sibling containers.
- **Relays can't reach `http://nomad:4646`** — the relay containers must be on
  `qumo-net` (they are, via `network_mode`); verify with
  `nomad alloc exec <id> wget -qO- http://nomad:4646/v1/agent/health`.
