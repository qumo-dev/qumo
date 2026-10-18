// Package hls serves a MoQ track as HLS and DASH by feeding it through
// qumo-ledger.
//
// [Run] connects to a MoQ relay, subscribes to a track, and stores each
// received group in a qumo-ledger track as a sealed group, its frames exactly
// as MoQ carried them (moq-lite-05, qumo-ledger decision 15); the HTTP handler
// renders that track as an HLS playlist and a DASH MPD over HTTP. qumo-ledger
// only stores the track: the HLS/DASH views over it live in this package.
//
// Packaging happens on read, not at the publisher and not at ingest. MoQ
// carries LOC — an encoded frame carrying a microsecond timestamp and almost
// nothing else — which [internal/cmaf] turns into CMAF when a viewer asks for a
// segment: one moof+mdat fragment per stored group, in a microsecond timescale
// so the LOC timestamps carry unrounded. A group's media extent is measured
// when it is stored, from the gaps between its frames' LOC timestamps; the
// last frame, whose successor belongs to the next group, is stated as the
// group's mean interval, and the fragment covers exactly that extent. The wall
// clock is anchored once, on the first group, and every group after it is
// placed by its media time.
package hls
