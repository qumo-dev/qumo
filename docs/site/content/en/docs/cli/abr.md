---
title: abr
description: Transcode broadcasts into an adaptive-bitrate ladder and republish them through the relay.
weight: 5
---

Transcodes the broadcasts on a relay into an adaptive-bitrate (ABR) ladder.
For every broadcast announced beneath `ABR_PREFIX`, it publishes an ABR
broadcast beside it, at the same path with `ABR_SUFFIX` appended
(`/live/cam1` → `/live/cam1.abr`). The ABR broadcast carries the source's
tracks as they are, plus one rendition per rung of the ladder that is shorter
than the source video, encoded by [ffmpeg](https://ffmpeg.org/).

An alias is not transcoded: a broadcast that answers subscriptions with a
redirect to another, such as the HLS opt-in a publisher announces beneath
`/.hls/`, is another name for the broadcast it points to, which is announced
and transcoded at its own path.

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
$ RELAY_URL=https://relay:4433 ABR_PREFIX=/live/ ABR_LADDER=720:2500,360:800 qumo abr
INFO abr: starting relay=https://relay:4433 prefix=/live/ suffix=.abr ladder=720:2500,360:800 codec=libx264
INFO abr: connected to the relay
INFO abr: transcoding source=/live/cam1 abr=/live/cam1.abr
```

A 1080p source at `/live/cam1` then has `/live/cam1.abr` beside it, whose
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

- The source video must be H.264 (`avc1`) **without B-frames**: LOC carries
  presentation timestamps only, so frames must arrive in presentation order.
  OBS and ffmpeg in their low-latency settings (`-bf 0`, `-tune zerolatency`)
  qualify.
- `ABR_CODEC` must be an H.264 encoder, so browsers decode every rendition
  with the same decoder.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `RELAY_URL` | `https://localhost:4433` | The relay to transcode broadcasts of, dialed as a client. It may carry a credential as `?jwt=` that grants subscribing to the sources and publishing the ABR broadcasts. |
| `RELAY_CA_FILE` | (unset) | PEM certificate to trust as the relay's root, instead of the system roots. |
| `RELAY_TLS_INSECURE` | `false` | Skip relay TLS verification, for a self-signed dev relay. |
| `ABR_PREFIX` | `/` | The directory whose broadcasts are transcoded. |
| `ABR_SUFFIX` | `.abr` | Appended to a source's path to name its ABR broadcast. Broadcasts already ending in it are not transcoded again, nor are aliases (broadcasts that redirect subscribers to another). |
| `ABR_LADDER` | `720:2500,360:800` | The renditions: comma-separated `height:kbps`. Rungs as tall as the source or taller are skipped. |
| `ABR_FFMPEG` | `ffmpeg` | The ffmpeg executable. |
| `ABR_CODEC` | `libx264` | The ffmpeg H.264 encoder, e.g. `h264_nvenc` or `h264_vaapi` for hardware encoding. |
| `ABR_PRESET` | `veryfast` | The encoder's `-preset`; set it to one the encoder knows (e.g. `p1` for `h264_nvenc`), or empty for none. |
| `QUIC_*` | | QUIC transport settings of the session to the relay; see [Configuration]({{< relref "../configuration" >}}). |
