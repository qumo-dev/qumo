---
title: Docker
description: Run the qumo relay as a container.
weight: 1
---

qumo publishes prebuilt multi-arch images to GHCR — no Dockerfile of your own needed.

```bash
docker pull ghcr.io/qumo-dev/qumo:latest

docker run -d \
  --name qumo-relay \
  -p 4433:4433/udp \
  -p 8080:4433 \
  -v "$PWD/certs:/app/certs:ro" \
  -e CERT_FILE=certs/server.crt \
  -e KEY_FILE=certs/server.key \
  -e RELAY_NAME=relay-1 \
  ghcr.io/qumo-dev/qumo:latest relay --role hub
```

The container listens on `4433` for QUIC (UDP) and serves HTTP health/metrics
on the same port (TCP). All configuration is environment variables — see
[Configuration]({{< relref "../configuration" >}}).

## Compose

A minimal single-relay `docker-compose.yml`:

```yaml
services:
  relay:
    image: ghcr.io/qumo-dev/qumo:latest
    command: ["relay"]
    ports:
      - "4433:4433/udp"
      - "8080:4433"
    environment:
      RELAY_ADDR: "0.0.0.0:4433"
      CERT_FILE: certs/server.crt
      KEY_FILE: certs/server.key
    volumes:
      - ./certs:/app/certs:ro
```

For a multi-node mesh (hub + edge, one or more regions), every node runs the
same image — `RELAY_NAME`, `--role`, `PEERS` and the relay's own peer
certificate (`PEER_CERT_FILE`, `PEER_KEY_FILE`) differ per service; the relay
CA (`CA_FILE`) is the same for all. See
[Peer topology]({{< relref "peer-topology" >}}) for how nodes discover and
connect to each other.
