# Single-region (asia) qumo cluster on Nomad.
#
# Purpose: exercise the static UPSTREAM_ADDR edge->hub topology (see
# internal/relay/server.go ConnectPeers) on real Nomad-launched containers,
# complementing docker-compose.static.yml's plain-Docker-Compose version.
#
# There is no dynamic peer discovery; edges are pointed at the hubs via
# a fixed UPSTREAM_ADDR list, resolved through Docker's embedded DNS
# using each hub's network_aliases on the shared "qumo-net" network.
#
# Cross-region hub<->hub is out of scope here — see docker/nomad/README.md.

job "qumo-cluster" {
  datacenters = ["dc1"]
  type        = "service"

  # ───────────────────────────── Hubs ─────────────────────────────
  group "hubs" {
    count = 2

    network {
      # Service registration port. With address_mode = "driver" on the task
      # service below, the service registers as <container-ip-on-qumo-net>:4433.
      port "moqt" {
        to = 4433
      }
    }

    task "relay" {
      driver = "docker"

      config {
        image        = "qumo:local" # build first: docker build -f docker/Dockerfile -t qumo:local .
        network_mode = "qumo-net"   # share the compose network with edges + Nomad
        ports        = ["moqt"]
        # network_aliases gives each hub a stable, per-index DNS name on
        # qumo-net (Docker's embedded DNS resolves it for other containers on
        # the same user-defined network) so edges can dial it by name instead
        # of a discovered/dynamic address.
        network_aliases = ["hub-${NOMAD_ALLOC_INDEX}"]
        args            = ["relay", "--role", "hub"]
      }

      # Task-level service (required for address_mode = "driver").
      service {
        name         = "qumo-relay"
        port         = "moqt"
        address_mode = "driver" # register the container's qumo-net IP, not the host
        tags         = ["role=hub", "region=asia"]
      }

      env {
        RELAY_ADDR = "0.0.0.0:4433"
        RELAY_NAME = "hub-asia-${NOMAD_ALLOC_INDEX}"
      }

      resources {
        cpu    = 100
        memory = 128
      }
    }
  }

  # ───────────────────────────── Edges ─────────────────────────────
  group "edges" {
    count = 2

    network {
      port "moqt" {
        to = 4433
      }
    }

    task "relay" {
      driver = "docker"

      config {
        image        = "qumo:local"
        network_mode = "qumo-net"
        ports        = ["moqt"]
        args         = ["relay", "--role", "edge"]
      }

      service {
        name         = "qumo-relay"
        port         = "moqt"
        address_mode = "driver"
        tags         = ["role=edge", "region=asia"]
      }

      env {
        RELAY_ADDR = "0.0.0.0:4433"
        RELAY_NAME = "edge-asia-${NOMAD_ALLOC_INDEX}"
        # Static upstream list: both hub aliases from the "hubs" group above.
        # Fixed to match that group's count = 2; bump both together.
        UPSTREAM_ADDR = "hub-0:4433,hub-1:4433"
      }

      resources {
        cpu    = 100
        memory = 128
      }
    }
  }
}
