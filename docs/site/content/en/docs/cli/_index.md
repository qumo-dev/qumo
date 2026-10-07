---
title: CLI reference
description: qumo's subcommands — relay, rtmp, rtsp, rtsp-push, hls, http, playground, doctor, loadgen, update, version.
weight: 5
cascade:
  type: docs
---

```
Usage: qumo <command>

Commands:
  relay      Start the MoQ relay server (--role hub|edge; default flat)
  rtmp       Start the RTMP ingest server
  rtsp       Pull from an RTSP source (e.g. IP camera) and republish as MoQT
  rtsp-push  Start the RTSP push ingest server (ANNOUNCE/RECORD)
  hls        Start the HLS/DASH egress server
  http       Start the HTTP ingest server (announce/record over POST, served as MoQT)
  playground Start a local demo (relay + web UI) on http://127.0.0.1:8080
  doctor     Explain effective runtime config (GC target) — read-only
  loadgen    Drive an out-of-process capacity load against a relay (subscribe|publish)
  update     Update qumo to the latest release (--check to just check)
  version    Print version information
```

All commands are configured via environment variables — see
[Configuration]({{< relref "../configuration" >}}).

{{< cards >}}
	{{< card link="relay" title="relay" icon="server" subtitle="Start the MoQ relay server." >}}
	{{< card link="rtmp" title="rtmp" icon="video-camera" subtitle="Standalone RTMP ingest server." >}}
	{{< card link="rtsp" title="rtsp" icon="cloud-download" subtitle="Pull an RTSP source (IP camera) into MoQT." >}}
	{{< card link="rtsp-push" title="rtsp-push" icon="cloud-upload" subtitle="Standalone RTSP push ingest server." >}}
	{{< card link="hls" title="hls" icon="globe-alt" subtitle="Serve a MoQ stream as HLS/DASH." >}}
	{{< card link="http" title="http" icon="upload" subtitle="Record POSTed records and serve them as MoQT." >}}
	{{< card link="playground" title="playground" icon="lightning-bolt" subtitle="One-command local demo." >}}
	{{< card link="doctor" title="doctor" icon="beaker" subtitle="Explain effective runtime config." >}}
	{{< card link="loadgen" title="loadgen" icon="chart-bar" subtitle="Out-of-process capacity load generator." >}}
	{{< card link="update" title="update" icon="refresh" subtitle="Self-update to the latest release." >}}
	{{< card link="version" title="version" icon="tag" subtitle="Print build-time version info." >}}
{{< /cards >}}
