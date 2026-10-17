---
title: hls
description: Start the HLS/DASH egress server.
weight: 5
---

The HLS/DASH egress subscribes to MoQ broadcasts, packages each received group
of LOC frames into CMAF (fMP4) fragments, writes them into qumo-ledger tracks,
and serves multi-track HLS and DASH over HTTP. Packaging happens at the subscriber:
the publisher sends LOC over MoQ, not CMAF.

Publishers opt into HLS egress by announcing their broadcast path with the `/.hls/`
prefix (e.g. `ANNOUNCE /.hls/live/cam1`). Media frames are **not double-published**:
the publisher uploads media only once to its canonical broadcast path (`/live/cam1`).
The egress listens for announcements, strips the `/.hls` prefix, reads the catalog at
`/live/cam1`, discovers its video tracks (audio tracks are not packaged yet), packages them into
independent ledger tracks, and dynamically mounts HTTP endpoints.

It verifies the relay's certificate by default.

## Usage

```
qumo hls
```

No flags — it is configured entirely through environment variables.

## Example

Subscribe to a relay and serve on the default address:

```console
$ qumo hls
INFO hls: serving addr=:8080 store=file:ledger announce_prefix=/.hls/
```

When a publisher announces `/.hls/live/cam1`, the egress dynamically routes endpoints
for the broadcast and its tracks:

```console
# Master playlist listing all tracks and variants
$ curl http://localhost:8080/live/cam1/master.m3u8
$ curl http://localhost:8080/live/cam1/playlist.m3u8   # convenience alias for players

# Track-specific media playlist and DASH MPD
$ curl http://localhost:8080/live/cam1/video/playlist.m3u8
$ curl http://localhost:8080/live/cam1/manifest.mpd
```

Segments are shared by both formats and addressed by group id
(`http://localhost:8080/live/cam1/video/<group-id>.m4s`); the fMP4 init segment is served at
`/live/cam1/video/init.m4s` and referenced from the playlist's `#EXT-X-MAP`. Point an HLS player
(such as `hls.js` or Safari) at `http://localhost:8080/live/cam1/playlist.m3u8`. If only a single
broadcast is active, root requests (`http://localhost:8080/playlist.m3u8`) redirect to its master playlist.
The broadcast-level `manifest.mpd` redirects to the default track's (`video`, else any) MPD.

## Opting in on a relay with auth on

Announcing `/.hls/<broadcast>` is what makes the egress serve a broadcast over
HTTP: to anyone who reaches it, unless [viewers are checked](#viewers). On a
relay with auth on, the opt-in is a grant the app issues deliberately, on a
token of its own:

- **Rooted under `/.hls/`.** The announcement is publishing at
  `/.hls/acme/room/42/cam`, which a token rooted at `/acme/room/42/` doesn't
  cover; the opt-in is a token rooted at, e.g., `/.hls/acme/room/42/` with
  `publish` `/cam`.
- **Signed by the app's own key.** A key confined to `/acme/` also signs for
  `/.hls/acme/`, so the opt-in needs no other key, and a client presents it
  as a ticket on the session it publishes on.
- **The app decides.** Neither the relay nor the egress checks that the
  bearer publishes `/acme/room/42/cam`: the egress subscribes with its own
  grant. Issue an opt-in only for a broadcast the app means to serve over
  HTTP.

```bash
# an opt-in for /acme/room/42/cam, signed by the app's key confined to /acme/
qumo auth token -key signing-key.jwk -root /.hls/acme/room/42/ -publish /cam
```

## Viewers

By default the egress serves every opted-in broadcast to anyone. With
`QUMO_AUTH_KEYS` — the same key set the relay reads — every request needs a
token that may `subscribe` to its broadcast, as `?jwt=` on the playlist or MPD
URL; the token a viewer would present to the relay works here too.

```bash
QUMO_AUTH_KEYS=keys.json qumo hls
# a viewer's token for /acme/room/42/cam, signed by the app's key
T=$(qumo auth token -key signing-key.jwk -root /acme/room/42/ -subscribe /cam)
# the player opens http://egress:8080/acme/room/42/cam/master.m3u8?jwt=$T
```

- **The token rides along.** Playlists declare it with
  `EXT-X-DEFINE:QUERYPARAM` (HLS protocol version 11), so the player repeats
  it on every variant playlist, init segment and segment; MPDs carry
  `UrlQueryInfo` (MPEG-DASH Annex I), which repeats the MPD URL's query on
  segment requests. The manifests are the same for every viewer.
- **Players.** hls.js and dash.js carry it; a player without `QUERYPARAM`
  support — ffmpeg, for one — requests the literal `{$jwt}` and is refused.
- **Checked on every request.** A missing or invalid token is answered `401`,
  one that may not watch the broadcast `403`, and `503` until the key set is
  loaded. A viewer's playback stops when its token expires (an hour at most):
  the player reopens the playlist URL with a fresh one.
- **Behind a CDN.** Checked responses are `Cache-Control: private`, so a
  shared cache does not serve one viewer's segments to another — and caches
  nothing. To cache, let the CDN check tokens at its edge (its own scheme,
  CTA-5007 Common Access Token, or RFC 9246 URI signing) and set only
  `HLS_TOKEN_PARAM` to the query parameter its tokens travel in: the
  manifests then carry that parameter, and the egress checks nothing itself.

## Storage: S3 object storage or filesystem

By default the egress writes fragments into a local filesystem directory (`./ledger`). For production deployments (e.g. multi-node egress with Cloudflare R2 or Amazon S3), configure `LEDGER_URI`:

```bash
# Cloudflare R2, Amazon S3, or MinIO
LEDGER_URI="s3://my-bucket/hls-streams?region=auto&endpoint=https://<account>.r2.cloudflarestorage.com" qumo hls

# Local filesystem directory
LEDGER_URI="file:///var/lib/qumo/ledger" qumo hls
```

## TLS

The egress verifies the relay's certificate against the system root store by
default. Trust a specific relay cert with `RELAY_CA_FILE`, or disable
verification for a self-signed dev relay with `RELAY_TLS_INSECURE=true`:

```bash
qumo hls                                   # verify against the system roots
RELAY_CA_FILE=certs/server.crt qumo hls    # trust this relay's cert
RELAY_TLS_INSECURE=true qumo hls           # dev relay with a self-signed cert
```

### Connecting to a relay with auth on

The egress is an HLS **origin**: viewers are authorized in front of it, by your
CDN or application, not by the relay. Against a relay with auth on
(`QUMO_AUTH_KEYS`), the egress itself connects in one of two ways:

- **With a signing key** (`RELAY_SIGNING_KEY`), like any client. The egress
  signs itself a fresh credential on every connect, granting `subscribe` and
  nothing else. It reads announcements under `/.hls/` and the broadcasts they
  name, so the key is one for the whole namespace (no `-prefix`) that the
  relay's key set trusts, made with `qumo auth keygen`; the egress refuses to
  start with a confined one. The relay reports the session's usage under that
  key.
- **As a relay peer** (`RELAY_CERT_FILE`, `RELAY_KEY_FILE`). The egress
  presents a peer certificate from the relay CA that the relay trusts as
  `CA_FILE`, one that carries the peering name. The relay then never checks
  the egress for a credential, and reports no usage for it. A peer is trusted
  with everything: it may subscribe to anything, and the relay would route a
  broadcast it announced. Issue the egress a peer certificate only where it
  runs under the same control as the relays.

```bash
# a signing key
RELAY_URL=https://relay.example.com:4433 \
RELAY_SIGNING_KEY=/secrets/egress-key.jwk qumo hls

# or a peer certificate
RELAY_URL=moqt://relay.example.com:4433 \
RELAY_CERT_FILE=/secrets/egress.crt RELAY_KEY_FILE=/secrets/egress.key \
RELAY_CA_FILE=/secrets/relay-ca.crt qumo hls
```

- **A credential in `RELAY_URL` (`?jwt=`) is for trying things out.** A token
  lives an hour at most, and what it grants with it: after the hour the
  session may subscribe to nothing, and every reconnect is refused. With the
  signing key, the egress signs a fresh credential on each connect and
  presents another on the session before the one in force expires.
- **One way at a time.** The relay never reads the credential of a session
  that presents a peer certificate, so the egress refuses to start with a
  certificate beside a signing key or a `?jwt=`.
- **A certificate of the egress's own, with the peering name.** A certificate
  from the CA without the name grants nothing: the relay asks that session
  for a credential. The egress refuses to start with one. It can't use the
  certificate of the relay it dials either: a relay drops a session that
  presents its own certificate, as itself.
- **Native QUIC only for a certificate:** only a native-QUIC session can be a
  peer, so a peer certificate needs a `moqt://` `RELAY_URL`. The egress
  refuses to start otherwise.
- **Renewal:** the certificate is read again on every reconnect, so a
  short-lived one renewed on disk is picked up without a restart. The signing
  key is read once, at startup.

The playground's HLS scenario opts its broadcast in by announcing it under
`/.hls/` as well. So does `cmd/seed-moq`, the dev seeder: a standalone server
(not a relay) that presents an ephemeral self-signed certificate and publishes
`/live/cam1`. Point the egress at it with `RELAY_TLS_INSECURE=true`; the default
`RELAY_URL=https://localhost:4433` already matches, and the stream is served at
`/live/cam1/master.m3u8`.

## Configuration

All configuration is via environment variables:

| Variable | Default | Description |
|---|---|---|
| `HLS_ADDR` | `:8080` | HTTP listen address. |
| `RELAY_URL` | `https://localhost:4433` | MoQ relay URL to subscribe to. Use `moqt://…` with `RELAY_CERT_FILE`. |
| `LEDGER_URI` | `file:ledger` | qumo-ledger store URI: `s3://bucket/prefix?region=...` (Amazon S3, Cloudflare R2, MinIO), `file:///path`, `postgres://...`, or `mem:`. |
| `HLS_WINDOW` | `12` | Segments kept in the manifest — the live window, and how far back a viewer can seek. `0` lists the whole track, making it a recording rather than a live stream. |
| `HLS_LIVE_TIMEOUT_S` | `10` | Seconds of silence after which the publisher is treated as gone; the feed resubscribes and manifests answer `503`. |
| `RELAY_CA_FILE` | _unset_ | PEM cert to trust as the relay's root, overriding the system roots. Unset means verify against the system root store. |
| `RELAY_TLS_INSECURE` | `false` | Skip relay TLS verification entirely. Dominates `RELAY_CA_FILE` when both are set. |
| `RELAY_SIGNING_KEY` | _unset_ | A signing key file (`qumo auth keygen`) the relay's key set trusts. The egress signs a fresh `subscribe`-only credential on every connect, in place of any `?jwt=` in `RELAY_URL`, and renews it on the session before it expires. The key must cover the whole namespace (no `-prefix`). Not with `RELAY_CERT_FILE`. |
| `RELAY_CERT_FILE`, `RELAY_KEY_FILE` | _unset_ | A peer certificate and key (PEM) for the egress, from the private CA the relay trusts as `CA_FILE`, with the peering name. It must be the egress's own, not the relay's. The relay serves the egress as a relay peer: no credential, no expiry, nothing checked. Not with `RELAY_SIGNING_KEY` or `?jwt=`. Set both or neither; they need a `moqt://` `RELAY_URL`. |
| `QUMO_AUTH_KEYS` | _unset_ | The key set viewers' tokens are verified against, as the relay reads it (a file or URL): every request needs a token that may `subscribe` to its broadcast. Unset lets every viewer in. See [Viewers](#viewers). |
| `QUMO_AUTH_KEYS_CACHE` / `QUMO_RELAY_TOKEN` | _unset_ | A file a downloaded key set is kept in, and the bearer token sent to a key-set URL. |
| `HLS_TOKEN_PARAM` | `jwt` with `QUMO_AUTH_KEYS`, else _unset_ | The query parameter a viewer's token travels in, carried by the manifests onto every URL they name: letters, digits, `-` and `_`. Set it alone for a CDN that checks its own tokens. |
| `CORS_ALLOWED_ORIGINS` | _unset_ | Comma-separated origins allowed to fetch manifests and segments, or `*` for any. Unset disables CORS. Required when the player is served from another origin (e.g. `http://localhost:5173` for the playground). |

## See also

- [relay]({{< relref "relay" >}}) — the MoQ relay the egress subscribes to.
- [playground]({{< relref "playground" >}}) — the local demo whose HLS scenario publishes CMAF for the egress to serve.
- [Configuration]({{< relref "../configuration" >}}) — the shared environment variable reference.
- [Deployment → TLS & mTLS]({{< relref "../deployment/tls" >}}) — generating the certificate the relay presents.
