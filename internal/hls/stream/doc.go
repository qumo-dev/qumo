// Package stream renders a [ledger] track over HTTP as HLS and DASH, packaging
// each stored group — its frames as MoQ carried them — into a CMAF fragment
// when it is asked for.
//
// Both are derived views over the same track, never the storage format itself:
// a Group is one segment, [ledger.GroupInfo.Duration] becomes HLS EXTINF and
// DASH @d, a new producer epoch becomes an HLS #EXT-X-DISCONTINUITY and a DASH
// timeline reset, and a wallclock anchor becomes #EXT-X-PROGRAM-DATE-TIME and
// the MPD availabilityStartTime. A [Handler] is an [http.Handler] over one
// [*ledger.Track] that routes playlist and manifest requests and serves the
// segments both point at, read through the ledger.
//
// By default the playlist is the whole track and grows as groups land — HLS as
// an EVENT playlist, DASH as a dynamic MPD — because the ledger is append-only
// and never deletes. [Options.Window] instead caps it at the most recent
// segments, which makes the HLS playlist a sliding live one (EVENT forbids
// removing segments) and gives the MPD a timeShiftBufferDepth. Rolling out of a
// manifest is not deletion: an older segment stays addressable for a client that
// still holds its URL. [Options.EpochWindow] bounds it along the other axis, in
// producer lifetimes — a live viewer has no reason to be shown the session
// before a restart, and left listed it is where a player starts. Either way there
// is no end-list signal; the ledger has no notion of a finished track.
//
// Each group must carry a positive Duration: a segment without one has no HLS
// EXTINF or DASH @d, so the renderers refuse it.
//
// A manifest is regenerated per request and the track is walked each time, which
// is O(track size) even when [Options.Window] bounds the output — a sliding
// playlist's EXT-X-MEDIA-SEQUENCE has to count what preceded it.
package stream
