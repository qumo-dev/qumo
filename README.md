# qumo

[![CI](https://github.com/qumo-dev/qumo/actions/workflows/ci.yml/badge.svg)](https://github.com/qumo-dev/qumo/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/qumo-dev/qumo)](https://goreportcard.com/report/github.com/qumo-dev/qumo)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Docs](https://img.shields.io/badge/docs-qumo--dev.github.io-blue)](https://qumo-dev.github.io/qumo/)

**qumo** is a high-performance Media over QUIC (MoQ) relay server with peer-based content discovery, enabling distributed media streaming over the QUIC transport protocol.

📚 **Documentation:** [https://qumo-dev.github.io/qumo/](https://qumo-dev.github.io/qumo/)

## Features

- 🚀 **High-Performance Relay**: a MoQT relay built on QUIC
- 📡 **MoQT Protocol**: Media over QUIC Transport (moq-lite draft-04) via [gomoqt](https://github.com/qumo-dev/gomoqt)
- 🔗 **Peer-Based Topology**: relays connect to each other via `ANNOUNCE_PLEASE` for decentralized content discovery
- 📊 **Observability**: Prometheus metrics, health probes, and status APIs
- 🔒 **TLS Security**: TLS 1.3 by default; **clients verify the relay's certificate by default**
- 🐳 **Docker**: env-var zero-config; prebuilt multi-arch images on GHCR (`ghcr.io/qumo-dev/qumo`)

## Installation

#### Option 1: One-Line Installer (Recommended)

**Windows (PowerShell):**
```powershell
powershell -ExecutionPolicy ByPass -c "irm https://raw.githubusercontent.com/qumo-dev/qumo/main/install.ps1 | iex"
```

**Linux & macOS:**
```bash
curl -fsSL https://raw.githubusercontent.com/qumo-dev/qumo/main/install.sh | sh
```

#### Option 2: Install via Go

```bash
go install github.com/qumo-dev/qumo@latest
```

#### Option 3: Download a Binary Release

Download an archive from [GitHub Releases](https://github.com/qumo-dev/qumo/releases) and extract it. Binary releases are produced for Linux (amd64/arm64), macOS (amd64/arm64), and Windows (amd64/arm64).

```bash
# Linux/macOS example
tar xzf qumo_<version>_linux_amd64.tar.gz
./qumo version
```

#### Option 4: Docker

See [docker/README.md](docker/README.md) for compose examples, GHCR usage, and deployment options.

#### Option 5: Build from Source

Requires **Go 1.27+**, **[Deno](https://deno.land/)** (`mage build` embeds the web UI built with Deno + Vite) and **Mage** (`go install github.com/magefile/mage@latest`; `mage help` lists the targets).

```bash
git clone https://github.com/qumo-dev/qumo.git
cd qumo
mage build        # builds bin/qumo with version info (also embeds the web UI)
# or: go build -o qumo .
```

## Usage

```bash
qumo relay         # Start a MoQ relay server (QUIC/MoQT, WebTransport, WebSocket, peer mesh)
qumo rtmp          # Start an RTMP ingest server (bridges RTMP → MoQT over TCP :1935)
qumo rtsp          # Pull from an RTSP source (e.g. an IP camera) and republish as MoQT
qumo rtsp-push     # Start the RTSP push ingest server (TCP :8554; bridges RTSP → MoQT)
qumo hls           # Start the HLS/DASH egress server (subscribes to a relay, serves HLS/DASH)
qumo funnel        # Funnel many HTTP senders into one MoQT track (announce + POST records)
qumo playground    # One-command local demo: in-process relay + embedded web UI on http://127.0.0.1:8080
qumo loadgen       # Out-of-process capacity load generator (publish|subscribe) — see [docs/perf](docs/perf/README.md#benchmarking--capacity)
qumo auth          # Set up the credentials a relay verifies (keygen / token)
qumo doctor        # Explain effective runtime config (GC target) — read-only
qumo update        # Update qumo to the latest release (--check to verify without applying)
qumo version       # Print build-time version info
```

Configuration is via environment variables; see `relay-config.example.env`.

## Documentation & where to look

- **TLS & client verification** — [docs/site/content/en/docs/deployment/tls.md](docs/site/content/en/docs/deployment/tls.md); in short, servers present TLS from `CERT_FILE`/`KEY_FILE`, clients verify by default, and `qumo hls` verifies against the system roots (trust a specific cert with `RELAY_CA_FILE`, or use `RELAY_TLS_INSECURE=true` for a self-signed dev relay).
- **Relay peers / mutual TLS** — same [deployment/tls.md](docs/site/content/en/docs/deployment/tls.md) doc; relays authenticate via a relay CA (`CA_FILE`, `PEER_CERT_FILE`, `PEER_KEY_FILE`), separate from the public certificate they serve browsers.
- **Docker** — [docker/README.md](docker/README.md).
- **Relay implementation** — [internal/relay/README.md](internal/relay/README.md).
- **Build system (Mage)** — [magefiles/README.md](magefiles/README.md).
- **Demo server & embedded web UI** — [playground/README.md](playground/README.md).
- **Performance docs, benchmarking & capacity** — [docs/perf/README.md](docs/perf/README.md).
- **CHANGELOG** — [CHANGELOG.md](CHANGELOG.md).

## License

[Apache 2.0](LICENSE)
