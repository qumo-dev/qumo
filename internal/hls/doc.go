// Package hls serves a MoQ track as HLS and DASH by feeding it through
// qumo-ledger.
//
// [Run] connects to a MoQ relay, subscribes to a track, and writes each
// received group into a qumo-ledger track as a sealed group; [stream.Handler]
// renders that track as an HLS playlist and a DASH MPD over HTTP. qumo-ledger
// only stores the track: the HLS/DASH views over it live here, in
// internal/hls/stream.
//
// Packaging happens at the subscriber, not the publisher. MoQ carries LOC — an
// encoded frame carrying a microsecond timestamp and almost nothing else — which
// [internal/cmaf] turns into CMAF: one moof+mdat fragment per MoQ group, in a
// microsecond timescale so the LOC timestamps carry unrounded. A group's media
// extent is measured from the gaps between its frame timestamps rather than
// assumed from configuration; the last frame of a group, whose successor belongs
// to the next group and has not arrived, takes the mean of the others. The wall
// clock is anchored once, on the first group, and every group after it is placed
// by its media time.
package hls
