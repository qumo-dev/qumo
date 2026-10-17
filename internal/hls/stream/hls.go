package stream

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/okdaichi/qumo-ledger/ledger"
)

// renderOpts carries the rendering inputs that come from neither the schema nor
// the groups themselves: the segment URL extension and the init-segment URI.
type renderOpts struct {
	segExt  string // e.g. ".m4s"
	initURI string // relative init URI, "" when there is none

	// initServed reports that the handler serves the init segment at initURI,
	// rather than initURI being an external URL given as-is.
	initServed bool

	// queryParam names the manifest URL's query parameter clients carry onto
	// the URIs the handler serves, "" for none (see Options.QueryParam).
	queryParam string

	// sliding reports that the groups are a window over the track rather than
	// the whole of it, so the manifest describes a rolling live presentation
	// instead of an append-only one.
	sliding bool

	// mediaSequence is the media sequence number of the first listed segment —
	// the count of segments that have rolled out of the window.
	mediaSequence uint64

	// discontinuitySequence is the count of timeline resets that have rolled out
	// of the window, which a sliding playlist states so a client's discontinuity
	// numbering survives segments leaving.
	discontinuitySequence uint64

	// precedingEpoch is the epoch of the segment just before the window, or zero
	// when nothing precedes it. A window can open exactly on an epoch change,
	// where the reset belongs to the first listed segment and is invisible from
	// the listed groups alone.
	precedingEpoch uint64
}

// renderHLS builds a media playlist for groups in commit order.
//
// Unwindowed, the playlist is the whole track and grows as groups land: EVENT
// means segments are only ever appended, never removed. Windowed, it is a
// sliding live playlist — no EXT-X-PLAYLIST-TYPE, because segments leave the
// playlist as it rolls, which EVENT forbids. Neither carries EXT-X-ENDLIST: the
// ledger has no notion of a finished track.
//
// The producer's own — gappy, epoch-resetting — sequence is not the media
// sequence: HLS requires consecutive media sequence numbers, so the sequence is
// synthetic (the segment's ordinal), and the stable per-segment address is the
// GroupID carried in the URL.
func renderHLS(schema ledger.TrackSchema, groups []ledger.GroupInfo, opts renderOpts) ([]byte, error) {
	for _, g := range groups {
		if g.Duration <= 0 {
			return nil, fmt.Errorf("stream: group %s has no duration; HLS needs an EXTINF", g.ID)
		}
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	if opts.queryParam != "" {
		// QUERYPARAM needs protocol version 11.
		b.WriteString("#EXT-X-VERSION:11\n")
		fmt.Fprintf(&b, "#EXT-X-DEFINE:QUERYPARAM=\"%s\"\n", opts.queryParam)
	} else {
		b.WriteString("#EXT-X-VERSION:6\n")
	}
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", maxSegmentSeconds(schema, groups))
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", opts.mediaSequence)
	if opts.discontinuitySequence > 0 {
		// Without this, a client numbers discontinuities from zero and
		// mis-associates them once a reset has rolled out of the playlist.
		fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", opts.discontinuitySequence)
	}
	if !opts.sliding {
		b.WriteString("#EXT-X-PLAYLIST-TYPE:EVENT\n")
	}
	if opts.initURI != "" {
		initURI := opts.initURI
		if opts.initServed {
			initURI = opts.carry(initURI)
		}
		fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s\"\n", initURI)
	}

	// A window can begin exactly where a producer lifetime did, in which case the
	// reset belongs to the first listed segment. Seeding from what precedes the
	// window states it; unwindowed, nothing precedes the first segment and there
	// is no reset to announce.
	prevEpoch := opts.precedingEpoch
	for i, g := range groups {
		// A new producer lifetime is a discontinuity: the timeline reset.
		if (i > 0 || prevEpoch != 0) && g.ID.Epoch() != prevEpoch {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if g.Wallclock != 0 {
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n",
				time.Unix(0, g.Wallclock).UTC().Format(time.RFC3339Nano))
		}
		fmt.Fprintf(&b, "#EXTINF:%s,\n", extinfSeconds(g.Duration, schema.Timescale))
		fmt.Fprintf(&b, "%s\n", opts.carry(g.ID.String()+opts.segExt))
		prevEpoch = g.ID.Epoch()
	}

	return []byte(b.String()), nil
}

// carry returns uri, a URI the handler serves, with the query parameter the
// playlist declares (queryParam) appended as an HLS variable reference, so
// the client substitutes the value from the playlist's own URL. It returns
// uri as it is without one.
func (o renderOpts) carry(uri string) string {
	if o.queryParam == "" {
		return uri
	}
	sep := "?"
	if strings.Contains(uri, "?") {
		sep = "&"
	}
	return uri + sep + o.queryParam + "={$" + o.queryParam + "}"
}

// maxSegmentSeconds is the ceiling, in whole seconds, of the longest segment —
// the bound HLS TARGETDURATION must honor and a sensible DASH minBufferTime.
func maxSegmentSeconds(schema ledger.TrackSchema, groups []ledger.GroupInfo) int {
	var max float64
	for _, g := range groups {
		if g.Duration <= 0 {
			continue
		}
		if s := float64(g.Duration) / float64(schema.Timescale); s > max {
			max = s
		}
	}
	return int(math.Ceil(max))
}

// windowSeconds is the total media extent of the listed groups, in whole
// seconds, rounded up — how far back a client can seek in a windowed manifest.
func windowSeconds(schema ledger.TrackSchema, groups []ledger.GroupInfo) int {
	var total int64
	for _, g := range groups {
		if g.Duration > 0 {
			total += g.Duration
		}
	}
	return int(math.Ceil(float64(total) / float64(schema.Timescale)))
}

// extinfSeconds formats a media extent as the decimal seconds HLS carries in
// EXTINF. Three decimals is more than any player needs and keeps output stable.
func extinfSeconds(duration int64, timescale uint32) string {
	return strconv.FormatFloat(float64(duration)/float64(timescale), 'f', 3, 64)
}
