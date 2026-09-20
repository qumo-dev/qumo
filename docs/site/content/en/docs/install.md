---
title: Install
description: Install qumo via Go, prebuilt binary, Docker, or build from source.
weight: 1
---

qumo ships as a single static binary. Pick whichever install path fits —
these are alternatives, not sequential steps.

{{< tabs >}}

{{< tab name="Windows (PowerShell)" >}}
```powershell
powershell -ExecutionPolicy ByPass -c "irm https://raw.githubusercontent.com/qumo-dev/qumo/main/install.ps1 | iex"
```
Or via the documentation site:
```powershell
powershell -ExecutionPolicy ByPass -c "irm https://qumo-dev.github.io/qumo/install.ps1 | iex"
```
Downloads the Windows binary matching your architecture (x64 / ARM64), verifies the SHA-256 checksum, installs it to `~/.qumo/bin`, and adds it to your user `PATH`.
{{< /tab >}}

{{< tab name="Linux / macOS (Shell)" >}}
```bash
curl -fsSL https://raw.githubusercontent.com/qumo-dev/qumo/main/install.sh | sh
```
Or via the documentation site:
```bash
curl -fsSL https://qumo-dev.github.io/qumo/install.sh | sh
```
Detects your OS and CPU architecture, verifies SHA-256 checksums, installs to `~/.qumo/bin`, and guides you to add it to your `PATH`.
{{< /tab >}}

{{< tab name="Go install" >}}
```bash
go install github.com/qumo-dev/qumo@latest
```
{{< /tab >}}

{{< tab name="Binary release" >}}
Download the latest archive from [GitHub Releases](https://github.com/qumo-dev/qumo/releases):

```bash
# Linux/macOS (replace 0.5.0 with the latest version from the releases page)
curl -L https://github.com/qumo-dev/qumo/releases/download/v0.5.0/qumo_0.5.0_linux_amd64.tar.gz | tar xz
./qumo playground      # one-command demo: relay + web UI at http://127.0.0.1:8080

# Windows: download qumo_0.5.0_windows_amd64.zip from the releases page
```
{{< /tab >}}

{{< tab name="Docker" >}}
Prebuilt multi-arch images are published to GHCR — see
[Deployment → Docker]({{< relref "deployment/docker" >}}) for the pull/run
commands and compose examples.
{{< /tab >}}

{{< tab name="Build from source" >}}
```bash
git clone https://github.com/qumo-dev/qumo.git
cd qumo
mage build        # builds bin/qumo with version info
# or: go build -o qumo .
```

Requirements for building from source:

- **Go 1.27+** — on its own this is enough. `playground/dist` (the Vite bundles
  the binary embeds) is committed, so a plain `go build` produces a binary with
  the real web UI.
- **Deno** and **Mage** — only for `mage build`, which rebuilds the web UI
  before compiling. Install Mage with
  `go install github.com/magefile/mage@latest`. Needed if you are *changing*
  the playground UI; see `playground/README.md` for the rebuild-and-commit
  workflow.
{{< /tab >}}

{{< /tabs >}}

## Verify

```bash
qumo version
```

## Next

Once installed, see [Configuration]({{< relref "configuration" >}}) to set up
TLS and environment variables, then start a relay:

```bash
qumo relay
```
