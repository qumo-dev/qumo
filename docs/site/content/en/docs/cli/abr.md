---
title: abr
description: Transcode broadcasts into an adaptive-bitrate ladder and republish them through the relay.
weight: 5
---

Transcodes the broadcasts on a relay into an adaptive-bitrate (ABR) ladder.
A publisher opts a broadcast in as it opts one into [`qumo hls`]({{< relref "hls" >}}):
by announcing it again beneath `/.abr/` (`/.abr/live/cam1` for `/live/cam1`).
For every opt-in, `qumo abr` publishes an ABR broadcast beside the broadcast,
at the same path with `ABR_SUFFIX` appended (`/live/cam1.abr`). It carries the
source's tracks as they are, plus one rendition per rung of the ladder that is
shorter than the source video, encoded by [ffmpeg](https://ffmpeg.org/).

## Opting in

The opt-in carries no media. It answers every subscription with a redirect to
the broadcast, keeping the track name (`internal/optin`, the same handler as
the HLS opt-in). `qumo abr` reads the catalog through it and follows the
redirect only to the broadcast the opt-in names: an opt-in that redirects
elsewhere, or answers itself, is refused, so no one can have another
publisher's broadcast transcoded under their own name. Withdrawing the opt-in
withdraws the ABR broadcast; the broadcast itself plays on.

A publisher opts in only what it may publish, and the opt-in is a path of its
own. Unlike `/.hls/`, a signing key confined to a prefix
(`qumo auth keygen -prefix acme/`) does **not** also sign for the prefix's
mirror beneath `/.abr/`: transcoding costs the operator, so a tenant's key alone
does not let its publishers opt in.

It dials the relay as an ordinary client: it subscribes to the source like any
viewer and publishes the ABR broadcast like any publisher. The relay itself
never decodes media.

## Usage

```
qumo abr
```

Takes no flags or arguments — configured entirely through environment
variables. Needs `ffmpeg` (or `ABR_FFMPEG`) on the host.

## Example

```console
$ RELAY_URL=https://relay:4433 ABR_LADDER=720:2500,360:800 qumo abr
INFO abr: starting relay=https://relay:4433 opt_in=/.abr/ suffix=.abr ladder=720:2500,360:800 codec=libx264
INFO abr: connected to the relay
INFO abr: transcoding opt_in=/.abr/live/cam1 source=/live/cam1 abr=/live/cam1.abr
```

A 1080p source at `/live/cam1`, opted in at `/.abr/live/cam1`, then has
`/live/cam1.abr` beside it, whose
catalog lists:

| Track | Picture | Bitrate | From |
|---|---|---|---|
| `video` | 1080p | measured from the source's first groups | the source, forwarded as is |
| `video-720p` | 720p | 2.5 Mbit/s | encoded |
| `video-360p` | 360p | 800 kbit/s | encoded |
| `audio` | — | — | the source, forwarded as is |

## What a player receives

- **One alternate group.** Every video track has `altGroup: 1` in the MSF
  catalog, with its `width`, `height`, `bitrate`, codec and decoder
  configuration (`initRef`), so a player knows they are alternatives of one
  picture and what each costs.
- **Aligned groups.** Every rendition numbers its groups as the source does,
  and group N of each track starts at the same frame, with a keyframe. A
  player can leave one track after group N and join another at group N+1
  without a gap or a decoder reset beyond the new track's configuration.
- **Unchanged timestamps.** Each rendition frame carries the exact LOC
  timestamp (microseconds) of the source frame it was encoded from.

The source track is forwarded without re-encoding, so the top rendition has
the source's quality and latency. The renditions add the encoder's latency,
about 10 ms per frame with `libx264` `veryfast` on a modern CPU.

## How it encodes

One ffmpeg process per source decodes the source once and encodes every
rendition from it. Frames go to ffmpeg as fragmented MP4, one fragment per
frame, and come back as FLV, one pipe per rendition. Both are formats ffmpeg
reads and writes frame by frame, so neither adds a frame of delay. On Windows
each rendition has an ffmpeg of its own, because Windows cannot pass a process
the extra pipes.

When the encoder falls behind, source frames are dropped up to the next group,
where it picks up again from a keyframe. The forwarded source track is never
held up by the encoder.

## Requirements

- The source video must be one of:
  - H.264 (`avc1`) with its decoder configuration in the catalog and
    length-prefixed frames, as RTMP and RTSP ingest publish it;
  - H.264 (`avc1`) in Annex B, its parameter sets in the stream before each
    keyframe, as a browser's WebCodecs encoder in `annexb` format sends it:
    the configuration is read from the first keyframe, and the frames are
    rewritten with lengths for ffmpeg;
  - VP9 with a full codec string (`vp09.PP.LL.DD`) and its picture size in
    the catalog, as a browser's hardware encoder often sends it.

  Renditions are H.264 whatever the source. Another codec is forwarded
  without renditions.
- The source must have **no B-frames**: LOC carries presentation timestamps
  only, so frames must arrive in presentation order. OBS and ffmpeg in their
  low-latency settings (`-bf 0`, `-tune zerolatency`) and browser encoders in
  `realtime` mode qualify.
- `ABR_CODEC` must be an H.264 encoder, so browsers decode every rendition
  with the same decoder.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `RELAY_URL` | `https://localhost:4433` | The relay to transcode broadcasts of, dialed as a client. It may carry a credential as `?jwt=` that grants listing and subscribing to the opt-ins beneath `/.abr/` and the broadcasts they name, and publishing the ABR broadcasts. |
| `RELAY_SIGNING_KEY` | (unset) | A signing key (`qumo auth keygen`) the relay's key set trusts, for the whole namespace (prefix `/`). The transcoder signs a fresh credential with it on every dial, in place of any `?jwt=`, granting what it needs (subscribing anywhere, to read opt-ins and their broadcasts, and publishing the ABR broadcasts), and renews it on the session before it lapses. A credential fixed in `RELAY_URL` lapses with its token. |
| `RELAY_CA_FILE` | (unset) | PEM certificate to trust as the relay's root, instead of the system roots. |
| `RELAY_TLS_INSECURE` | `false` | Skip relay TLS verification, for a self-signed dev relay. |
| `ABR_SUFFIX` | `.abr` | Appended to a source's path to name its ABR broadcast. An opt-in for a broadcast already ending in it is ignored. |
| `ABR_LADDER` | `720:2500,360:800` | The renditions: comma-separated `height:kbps`. Rungs as tall as the source or taller are skipped. |
| `ABR_FFMPEG` | `ffmpeg` | The ffmpeg executable. |
| `ABR_CODEC` | `libx264` | The ffmpeg H.264 encoder, e.g. `h264_nvenc` or `h264_vaapi` for hardware encoding. |
| `ABR_PRESET` | `veryfast` | The encoder's `-preset`; set it to one the encoder knows (e.g. `p1` for `h264_nvenc`), or empty for none. |
| `QUIC_*` | | QUIC transport settings of the session to the relay; see [Configuration]({{< relref "../configuration" >}}). |
