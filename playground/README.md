# qumo — AV Streaming Demo

A browser demo for experiencing audio/video live streaming over
[MoQ (Media over QUIC)](https://datatracker.ietf.org/wg/moq/about/) / WebTransport,
powered by the qumo relay.

It publishes a camera or screen-share stream to the relay over WebTransport and
subscribes back to it, exercising the MoQ-MoQ echo pipeline end to end (video +
audio). A scenario picker at the top switches between that **Echo** pipeline and
subscribe-only **RTMP / RTSP ingest** pipelines. The same UI is the foundation
for the HLS egress scenario tracked in the [Demo UI Improvements milestone][milestone].

[milestone]: https://github.com/qumo-dev/qumo/milestone/1

## Run it

There are two ways to run the demo:

- **`qumo playground`** — the self-contained distribution/demo path. A single
  binary that generates a dev cert, starts the relay in-process, serves the
  embedded UI, and exposes runtime config via `/config` (no build-time
  `VITE_CERT_HASH` needed). Build it from the repo root:

  ```bash
  mage build          # builds the UI, then the binary with the UI embedded
  ./bin/qumo playground
  ```

  Then open <http://127.0.0.1:8080>. The dev cert is cached under your per-user
  cache dir (`os.UserCacheDir`/qumo/playground) and reused until it nears
  expiry; set `QUMO_PLAYGROUND_CERT_DIR` to override its location (e.g. to share
  a cert with `mage cert`'s `./certs`).

  The built UI (`playground/dist`) is **committed** so that `go install` and a
  plain `go build` embed the real UI — Go module archives contain only
  git-tracked files. If you change anything under `playground/src` (or the
  locked deps), run `mage webbuild` and commit the regenerated `dist/`; CI's
  "Web UI dist freshness" job rebuilds the UI and fails a PR whose committed
  dist doesn't match byte-for-byte.

  Because that check is byte-exact, rebuild with the same toolchain CI uses:

  - **deno 2.8.1** (pinned in `.github/workflows/ci.yml`, `release.yml` and
    `docker/Dockerfile`). A different deno usually produces identical output,
    but only this version is guaranteed to.
  - **No Node needed.** `deno task build` runs Vite on the Deno runtime via
    `@deno/vite-plugin`, with the npm deps resolved from `deno.lock`.
  - `mage webbuild` runs `deno install --frozen`, so it builds against exactly
    the locked dep set. To take a dependency bump, run `deno install` (without
    `--frozen`) in `playground/` by hand and commit the updated `deno.lock`
    together with the rebuilt `dist/`.

  If CI still reports a stale dist after a clean local rebuild, don't hand-edit
  the bundles: download the `webui-dist` artifact from the failed "Web UI dist
  freshness" run, unpack it over `playground/dist`, and commit that — those are
  the exact bytes the gate compares against.

  Flags (`qumo playground -h`):

  | Flag            | Default          | Purpose                                              |
  | --------------- | ---------------- | ---------------------------------------------------- |
  | `--ui-addr`     | `127.0.0.1:8080` | Address the UI HTTP server binds.                    |
  | `--relay-addr`  | `127.0.0.1:4433` | Address the relay WebTransport server binds.         |

  There is deliberately **no `--host` flag**. The browser-facing relay URL is
  derived at request time from whatever host the UI was opened at: open
  `http://localhost:8080` and `/config` returns `relayUrl: https://localhost:4433`;
  open `https://example.com` (behind a proxy) and it returns
  `https://example.com:4433`. So the relay target always matches the address in
  the address bar, with nothing to configure.

  ### Hosting publicly (behind your own TLS proxy)

  WebTransport requires a secure context, and `localhost` is the only HTTP host
  that counts — so to serve the demo on a public host, put a TLS-terminating
  reverse proxy (nginx/Caddy/Cloudflare) in front of the UI:

  ```bash
  # On the host: relay binds a public interface so its UDP/QUIC port is reachable;
  # UI stays loopback for the proxy to forward to.
  qumo playground --relay-addr 0.0.0.0:4433
  # Then: proxy  https://example.com  ->  127.0.0.1:8080
  #        (forward X-Forwarded-Host so /config picks up the public host)
  #        allow  UDP/4433  through your firewall for WebTransport.
  ```

  The browser loads the UI over HTTPS (from your proxy), fetches `/config`, and
  dials WebTransport to `https://example.com:4433`, pinning the dev cert by its
  SHA-256 hash. The pin is by hash, not hostname, so the localhost-minted cert
  works on a public host without regeneration.

- **`mage web`** — the frontend development path (Vite dev server + HMR). The
  demo needs a running relay plus the Vite dev server. From the **repo root**:

  ```bash
  # 1. Generate a WebTransport cert
  mage cert

  # 2. Terminal A — start the relay
  mage relay        # or: ./bin/qumo-relay

  # 3. Terminal B — start the demo
  mage web
  ```

  Then open <http://localhost:5173>.

`mage relay` is the dev wrapper: it applies dev-friendly defaults
(`RELAY_ADDR=:4433` and CORS allowing the Vite origins) when you haven't set
them, so it connects alongside `mage web`
out of the box. The standalone `qumo relay` binary keeps its secure defaults
(same-origin CORS); if you use it instead, set
`CORS_ALLOWED_ORIGINS=http://localhost:5173` to allow the dev UI.

`mage cert` prefers **mkcert** when it's on PATH: it signs a long-lived
localhost cert that chains to a trusted local root CA, so the browser trusts it
directly — no `VITE_CERT_HASH`, no 14-day expiry, no Vite restart. Install
mkcert with `brew install mkcert` / `winget install FiloSottile.mkcert` (see the
[mkcert README](https://github.com/FiloSottile/mkcert)). Set
`CERT_HOSTS=192.168.1.10,desktop.local mage cert` to add extra SANs if you reach
the demo from another device on the LAN. When mkcert is absent,
`mage cert` falls back to a 14-day self-signed cert and writes its SHA-256 to
`playground/.env` as `VITE_CERT_HASH`; in that fallback the hash **is** required,
because Chrome's `serverCertificateHashes` rejects the self-signed cert without
it. (`qumo playground` sidesteps both by serving the hash at runtime via
`/config`.)

### Configuration

Environment variables live in `playground/.env` (see `.env.example`):

| Variable          | Description                                              |
| ----------------- | -------------------------------------------------------- |
| `VITE_RELAY_URL`  | Relay WebTransport URL (must be HTTPS).                  |
| `VITE_CERT_HASH`  | SHA-256 (hex) of the relay cert. Set by `mage cert` in its self-signed fallback; **not needed** when `mage cert` uses mkcert (browser-trusted). |

The header title is a fixed `qumo` (not configurable).

## Scenarios

Pick a scenario with the segmented tabs at the top. Each scenario is a distinct
WebTransport origin, so switching tabs reconnects. The path field is shared,
editable, and shareable — **Copy link** produces a `?scenario=&path=` URL that
opens the demo on the exact same stream.

Every default path embeds a per-session unique token (`/<name>-<id>` for echo,
`/<scheme>/<id>` for ingest) so that on a **shared public relay** no two users
collide on the same broadcast. The RTMP/RTSP tabs show a push command whose
target embeds that same unique path.

| Scenario | Origin | Default path | What it exercises | Status |
| -------- | ------ | ------------ | ----------------- | ------ |
| Echo | `https://localhost:4433` | `/<name>-<id>` (editable) | Publish → relay → subscribe (MoQ-MoQ) | Working |
| RTMP ingest | `https://localhost:4443` | `/rtmp/<id>` | Subscribe to an RTMP-pushed stream | Working (#141) |
| RTSP ingest | `https://localhost:4543` | `/rtsp/<id>` | Subscribe to an RTSP-pushed stream | Working (#141) |
| HLS | — | — | Consume the relay's HLS egress | Blocked (#142) |

For the ingest scenarios, start the demo origins and push a test stream:

```bash
mage demo:up      # relay + rtmp + rtsp origins (generates cert if missing)
mage demo:push    # opt-in ffmpeg test-pattern pushers → /rtmp/demo, /rtsp/demo
```

The RTMP/RTSP tabs also show a copy-pasteable ffmpeg push command. All origins
share one `mage cert` certificate. With mkcert the single cert is
browser-trusted for every origin; in the self-signed fallback a single
`VITE_CERT_HASH` (from `mage cert`) validates them.

## Controls

- **Publish (Echo):** resolution (480p/720p/1080p), framerate (24/30/60), and
  bitrate (0.5–6 Mbps) picks. These shape the camera capture and the encoder;
  stop and restart to apply a change mid-session.
- **Subscribe:** mute, volume, and fullscreen. These are viewer controls only —
  MoQ is live, so there is no pause/seek/scrub.
- **Stats overlay:** while a stream is active, both boards show a live readout
  over the preview — resolution, fps, media bitrate, and (publish) encoder
  queue / (subscribe) RTT and decoder queue. Updated once per second.
- **DevTools:** a collapsible panel under the boards, closed until opened. It
  shows the media bitrate, a table of tracks (bitrate, frame rate, latest
  group, and how many groups ended complete, skipped, aborted or late), and a
  timeline of each track's groups: when each arrived, how it ended, and below
  that what was rendered. An "audio out" lane shows the audio buffer's level
  and marks each moment sound was lost. Pick 60, 10 or 2 seconds of history (at 2 seconds
  every audio group is its own mark), pause it, and point at a group to pick
  out its received bar and its rendered span together.

## Develop

```bash
deno task dev      # Vite dev server
deno task test     # unit tests (deno test)
deno task build    # unit tests + type-check (deno check) + production build to dist/
deno task preview  # preview the production build

Install deps first with `deno install` (the project is Deno-managed —
`deno.lock` is the source of truth; `npm install` cannot resolve the
`@deno/vite-plugin` jsr deps).
```

## Project layout

```
src/
  App.tsx              App shell + header
  Dashboard.tsx        Scenario + path state, URL deep links, top controls
  ScenarioView.tsx     Per-scenario session, connection status, board layout
  ScenarioPicker.tsx   Echo / RTMP / RTSP tab selector (#137)
  PathControl.tsx      Shared editable path + copy/share (#137)
  PushInstructions.tsx ffmpeg push command for ingest scenarios (#141)
  ConnectionStatus.tsx WebTransport lifecycle indicator (#134)
  scenarios.ts         Scenario registry (ports, modes, push commands)
  cert.ts              VITE_CERT_HASH parsing + transport options
  publish/             Publish board: capture → encode → MoQ
  subscribe/           Subscribe board: start/stop, controls, stats overlay
  player/              Viewer: MoQ → decode → canvas, with no UI of its own.
                       Reads tracks through its own small interface
                       (source.ts); moq_source.ts adapts @qumo/moq to it
    audio/             WebCodecs decode + jitter buffer on the audio thread
    video/             WebCodecs decode + timed drawing on the canvas
  devtools/            DevTools panel and the recorder that feeds it
  user/                Random-name helper (seeds the Echo default path)
```
