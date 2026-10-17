package stream

import (
	"fmt"
	"strings"
	"time"

	"github.com/okdaichi/qumo-ledger/ledger"
)

// renderDASH builds a dynamic Media Presentation Description (MPD) for groups in
// commit order.
//
// The presentation is dynamic — it grows as groups land, mirroring an HLS EVENT
// playlist — and lists every committed segment, since the ledger never deletes.
// Segments are listed explicitly by their GroupID URL (a SegmentList of
// SegmentURL entries), so the same segment handler serves HLS and DASH; DASH
// offers no template variable for a producer's own id, so the list is explicit
// rather than templated. A SegmentTimeline alongside carries each segment's
// duration and resets @t at each epoch boundary, where media time restarts.
func renderDASH(schema ledger.TrackSchema, groups []ledger.GroupInfo, opts renderOpts) ([]byte, error) {
	for _, g := range groups {
		if g.Duration <= 0 {
			return nil, fmt.Errorf("stream: group %s has no duration; DASH needs an @d", g.ID)
		}
	}

	buffer := maxSegmentSeconds(schema, groups)

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" `)
	if opts.queryParam != "" {
		b.WriteString(`xmlns:up="urn:mpeg:dash:schema:urlparam:2014" `)
	}
	b.WriteString(`type="dynamic" ` +
		`profiles="urn:mpeg:dash:profile:isoff-live:2011"`)
	if ast := dashAvailabilityStartTime(schema, groups); ast != "" {
		fmt.Fprintf(&b, ` availabilityStartTime="%s"`, ast)
	}
	fmt.Fprintf(&b, ` minimumUpdatePeriod="%s"`, dashDuration(2))
	fmt.Fprintf(&b, ` minBufferTime="%s"`, dashDuration(buffer))
	fmt.Fprintf(&b, ` maxSegmentDuration="%s"`, dashDuration(buffer))
	// A windowed presentation only lists its most recent segments, which is
	// exactly what timeShiftBufferDepth describes: how far back a client may
	// seek. Without it a client may assume the whole presentation is listed.
	if opts.sliding {
		fmt.Fprintf(&b, ` timeShiftBufferDepth="%s"`, dashDuration(windowSeconds(schema, groups)))
	}
	b.WriteString(">\n")

	b.WriteString(`  <Period id="0" start="PT0S">` + "\n")
	fmt.Fprintf(&b, `    <AdaptationSet id="0" mimeType="%s" contentType="%s" segmentAlignment="true">`+"\n",
		schema.MIME, dashContentType(schema.MIME))
	if opts.queryParam != "" {
		// Annex I URL parameters: the client repeats the MPD URL's query on
		// its segment requests. Essential, so a client that can't honor it
		// skips the set rather than fetching segments without the parameter.
		b.WriteString(`      <EssentialProperty schemeIdUri="urn:mpeg:dash:urlparam:2014">` + "\n")
		b.WriteString(`        <up:UrlQueryInfo queryTemplate="$querypart$" useMPDUrlQuery="true"/>` + "\n")
		b.WriteString("      </EssentialProperty>\n")
	}
	b.WriteString(`      <Representation id="0">` + "\n")
	fmt.Fprintf(&b, `        <SegmentList timescale="%d">`+"\n", schema.Timescale)
	if opts.initURI != "" {
		fmt.Fprintf(&b, `          <Initialization sourceURL="%s"/>`+"\n", opts.initURI)
	}

	// SegmentTimeline: @t resets at each epoch boundary; otherwise the timeline
	// continues from the previous segment's end. Entries correspond 1:1, in
	// order, with the SegmentURL list below.
	b.WriteString("          <SegmentTimeline>\n")
	var prevEpoch uint64
	for i, g := range groups {
		if i == 0 || g.ID.Epoch() != prevEpoch {
			fmt.Fprintf(&b, `            <S t="%d" d="%d"/>`+"\n", g.MediaTime, g.Duration)
		} else {
			fmt.Fprintf(&b, `            <S d="%d"/>`+"\n", g.Duration)
		}
		prevEpoch = g.ID.Epoch()
	}
	b.WriteString("          </SegmentTimeline>\n")

	for _, g := range groups {
		fmt.Fprintf(&b, `          <SegmentURL media="%s%s"/>`+"\n", g.ID, opts.segExt)
	}

	b.WriteString("        </SegmentList>\n")
	b.WriteString("      </Representation>\n")
	b.WriteString("    </AdaptationSet>\n")
	b.WriteString("  </Period>\n")
	b.WriteString("</MPD>\n")

	return []byte(b.String()), nil
}

// dashAvailabilityStartTime is the wallclock the presentation began: the first
// anchored group's wallclock, less its own offset along the media timeline. A
// dynamic MPD needs one to relate media time to the wall clock; without any
// anchor there is none to report.
//
// Subtracting the media time is what keeps it stable. Reporting the anchor
// itself names when the *window* began, which for a windowed manifest moves
// every time the window rolls — and a moving availabilityStartTime shifts the
// whole timeline under a client on each refresh. Any group in an epoch yields
// the same answer, so the value holds as segments come and go.
func dashAvailabilityStartTime(schema ledger.TrackSchema, groups []ledger.GroupInfo) string {
	for _, g := range groups {
		if g.Wallclock == 0 {
			continue
		}
		start := time.Unix(0, g.Wallclock).Add(-mediaOffset(g.MediaTime, schema.Timescale))
		return start.UTC().Format(time.RFC3339Nano)
	}
	return ""
}

// mediaOffset converts a media-time extent into a duration. Seconds and
// remainder are converted separately so a long presentation cannot overflow the
// nanosecond multiply.
func mediaOffset(mediaTime int64, timescale uint32) time.Duration {
	if timescale == 0 {
		return 0
	}
	ts := int64(timescale)
	seconds := mediaTime / ts
	remainder := mediaTime % ts
	return time.Duration(seconds)*time.Second +
		time.Duration(remainder*int64(time.Second)/ts)
}

// dashDuration formats a whole-second extent as an ISO 8601 period.
func dashDuration(seconds int) string {
	return fmt.Sprintf("PT%dS", seconds)
}

// dashContentType projects a MIME type onto DASH's coarse content classification.
func dashContentType(mime string) string {
	switch {
	case strings.HasPrefix(mime, "video/"):
		return "video"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	default:
		return "application"
	}
}
