// Package stream renders a [ledger] track over HTTP as HLS and DASH.
//
// Both are derived views over the same protocol-agnostic manifest, never the
// storage format itself: a Group is one segment, [ledger.GroupInfo.Duration]
// becomes HLS EXTINF and DASH @d, a new producer epoch becomes an HLS
// #EXT-X-DISCONTINUITY and a DASH timeline reset, and a wallclock anchor becomes
// #EXT-X-PROGRAM-DATE-TIME and the MPD availabilityStartTime. A [Handler] is an
// [http.Handler] over one [*ledger.Track] that routes playlist and manifest
// requests and serves the segments both point at.
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
// # Segment delivery
//
// Delivery is pluggable at this layer. The default [ProxyResolver] streams
// segment bytes through the ledger via [ledger.Reader.ReadGroup] — fine for
// local development. A production deployment supplies a [RedirectResolver] that
// mints a signed URL from [ledger.GroupInfo.ObjectKey], so clients fetch objects
// directly and the store stays free of any presigning method it could not
// implement uniformly. Both formats address segments by their [ledger.GroupID],
// so one segment handler serves them both.
//
// # What the renderer assumes
//
// Each group must carry a positive Duration: a segment without one has no HLS
// EXTINF or DASH @d, so the renderers refuse it. Deriving that Duration from the
// media rather than assuming it is the writer's job — see the fmp4 package for
// the fragmented-MP4 case.
//
// The container is whatever the track's schema declares. A fragmented-MP4 track
// additionally requires [Options.InitSegment]: its segments carry no codec
// configuration, so a manifest without an init reference is unplayable, and
// [NewHandler] returns [ErrInitRequired] rather than serve one. A
// self-initializing container such as MPEG-TS needs none.
//
// A manifest is regenerated per request and the track is walked each time, which
// is O(track size) even when [Options.Window] bounds the output — a sliding
// playlist's EXT-X-MEDIA-SEQUENCE has to count what preceded it. Caching the
// rendered manifest, and a reader API that returns a window together with its
// ordinal, are deliberate follow-ons.
package stream
