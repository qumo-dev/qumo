package stream

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/okdaichi/qumo-ledger/ledger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testSchema = ledger.TrackSchema{
	Timescale: 90000,
	MIME:      "video/mp4",
	Encoding:  "fmp4",
}

// testBase anchors wallclock output at a fixed instant so the golden strings
// are reproducible. RFC3339Nano of it and its even-second offsets have no
// fractional part.
var testBase = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

// wallclock returns the Unix-nano anchor secs seconds after testBase.
func wallclock(secs int) int64 {
	return testBase.Add(time.Duration(secs) * time.Second).UnixNano()
}

// grp builds a GroupInfo with only the fields the renderers read.
func grp(id ledger.GroupID, media, dur, wall int64) ledger.GroupInfo {
	return ledger.GroupInfo{ID: id, MediaTime: media, Duration: dur, Wallclock: wall}
}

func TestRenderHLS_SingleEpoch(t *testing.T) {
	groups := []ledger.GroupInfo{
		grp(ledger.NewGroupID(1, 0), 0, 180000, 0),
		grp(ledger.NewGroupID(1, 1), 180000, 180000, 0),
	}

	body, err := renderHLS(testSchema, groups, renderOpts{segExt: ".m4s"})
	require.NoError(t, err)

	const want = `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-PLAYLIST-TYPE:EVENT
#EXTINF:2.000,
e000001-g00000000.m4s
#EXTINF:2.000,
e000001-g00000001.m4s
`
	assert.Equal(t, want, string(body))
}

// Two epochs exercise a discontinuity before epoch 2's first segment, the init
// map, and per-segment program-date-time — the rendering map from decision 14.
func TestRenderHLS_TwoEpochsDiscontinuity(t *testing.T) {
	groups := []ledger.GroupInfo{
		grp(ledger.NewGroupID(1, 0), 0, 180000, wallclock(0)),
		grp(ledger.NewGroupID(1, 1), 180000, 180000, wallclock(2)),
		grp(ledger.NewGroupID(2, 0), 0, 180000, wallclock(4)),
	}

	body, err := renderHLS(testSchema, groups, renderOpts{segExt: ".m4s", initURI: "init.m4s"})
	require.NoError(t, err)

	const want = `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-PLAYLIST-TYPE:EVENT
#EXT-X-MAP:URI="init.m4s"
#EXT-X-PROGRAM-DATE-TIME:2026-07-31T12:00:00Z
#EXTINF:2.000,
e000001-g00000000.m4s
#EXT-X-PROGRAM-DATE-TIME:2026-07-31T12:00:02Z
#EXTINF:2.000,
e000001-g00000001.m4s
#EXT-X-DISCONTINUITY
#EXT-X-PROGRAM-DATE-TIME:2026-07-31T12:00:04Z
#EXTINF:2.000,
e000002-g00000000.m4s
`
	assert.Equal(t, want, string(body))
}

func TestRenderHLS_NoDuration(t *testing.T) {
	groups := []ledger.GroupInfo{
		grp(ledger.NewGroupID(1, 0), 0, 0, 0), // a segment with no extent
	}

	_, err := renderHLS(testSchema, groups, renderOpts{segExt: ".m4s"})

	assert.Error(t, err, "a segment with no duration has no EXTINF to carry")
}

func TestRenderDASH_SingleEpoch(t *testing.T) {
	groups := []ledger.GroupInfo{
		grp(ledger.NewGroupID(1, 0), 0, 180000, wallclock(0)),
		grp(ledger.NewGroupID(1, 1), 180000, 180000, wallclock(2)),
	}

	body, err := renderDASH(testSchema, groups, renderOpts{segExt: ".m4s"})
	require.NoError(t, err)
	assertXMLWellFormed(t, body)

	const want = `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic" profiles="urn:mpeg:dash:profile:isoff-live:2011" availabilityStartTime="2026-07-31T12:00:00Z" minimumUpdatePeriod="PT2S" minBufferTime="PT2S" maxSegmentDuration="PT2S">
  <Period id="0" start="PT0S">
    <AdaptationSet id="0" mimeType="video/mp4" contentType="video" segmentAlignment="true">
      <Representation id="0">
        <SegmentList timescale="90000">
          <SegmentTimeline>
            <S t="0" d="180000"/>
            <S d="180000"/>
          </SegmentTimeline>
          <SegmentURL media="e000001-g00000000.m4s"/>
          <SegmentURL media="e000001-g00000001.m4s"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>
`
	assert.Equal(t, want, string(body))
}

// The epoch boundary resets @t to the new lifetime's media origin and lists the
// init segment, which DASH references through @initialization.
func TestRenderDASH_TwoEpochsInit(t *testing.T) {
	groups := []ledger.GroupInfo{
		grp(ledger.NewGroupID(1, 0), 0, 180000, wallclock(0)),
		grp(ledger.NewGroupID(1, 1), 180000, 180000, wallclock(2)),
		grp(ledger.NewGroupID(2, 0), 0, 180000, wallclock(4)),
	}

	body, err := renderDASH(testSchema, groups, renderOpts{segExt: ".m4s", initURI: "init.m4s"})
	require.NoError(t, err)
	assertXMLWellFormed(t, body)

	const want = `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic" profiles="urn:mpeg:dash:profile:isoff-live:2011" availabilityStartTime="2026-07-31T12:00:00Z" minimumUpdatePeriod="PT2S" minBufferTime="PT2S" maxSegmentDuration="PT2S">
  <Period id="0" start="PT0S">
    <AdaptationSet id="0" mimeType="video/mp4" contentType="video" segmentAlignment="true">
      <Representation id="0">
        <SegmentList timescale="90000">
          <Initialization sourceURL="init.m4s"/>
          <SegmentTimeline>
            <S t="0" d="180000"/>
            <S d="180000"/>
            <S t="0" d="180000"/>
          </SegmentTimeline>
          <SegmentURL media="e000001-g00000000.m4s"/>
          <SegmentURL media="e000001-g00000001.m4s"/>
          <SegmentURL media="e000002-g00000000.m4s"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>
`
	assert.Equal(t, want, string(body))
}

func TestRenderDASH_NoDuration(t *testing.T) {
	groups := []ledger.GroupInfo{
		grp(ledger.NewGroupID(1, 0), 0, 0, 0),
	}

	_, err := renderDASH(testSchema, groups, renderOpts{segExt: ".m4s"})

	assert.Error(t, err, "a segment with no duration has no @d to carry")
}

// assertXMLWellFormed confirms the MPD parses as XML. It is not a conformance
// check against the DASH schema, only that the renderer emits balanced markup.
func assertXMLWellFormed(t *testing.T, body []byte) {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		_, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return
		}
		require.NoError(t, err)
	}
}

// A track with no wallclock anchor anywhere has no instant to name as the
// presentation start. The MPD omits availabilityStartTime rather than inventing
// one: a wrong anchor shifts every client's timeline, which is worse than none.
func TestRenderDASH_NoWallclockAnchor(t *testing.T) {
	groups := []ledger.GroupInfo{
		grp(ledger.NewGroupID(1, 0), 0, 180000, 0),
		grp(ledger.NewGroupID(1, 1), 180000, 180000, 0),
	}

	body, err := renderDASH(testSchema, groups, renderOpts{segExt: ".m4s"})
	require.NoError(t, err)
	assertXMLWellFormed(t, body)

	assert.NotContains(t, string(body), "availabilityStartTime",
		"with no anchor to derive it from, the attribute is absent rather than wrong")
}

// DASH's coarse content classification projects from the track's MIME type: an
// audio track adapts as audio, and anything DASH has no class for adapts as
// application rather than being mislabeled video.
func TestRenderDASH_ContentType(t *testing.T) {
	groups := []ledger.GroupInfo{
		grp(ledger.NewGroupID(1, 0), 0, 180000, wallclock(0)),
	}

	for mime, want := range map[string]string{
		"audio/mp4": `contentType="audio"`,
		"video/mp4": `contentType="video"`,
		"text/ttml": `contentType="application"`,
	} {
		t.Run(mime, func(t *testing.T) {
			schema := testSchema
			schema.MIME = mime

			body, err := renderDASH(schema, groups, renderOpts{segExt: ".m4s"})
			require.NoError(t, err)
			assert.Contains(t, string(body), want)
		})
	}
}

// A query parameter is declared once and carried onto every URI the handler
// serves, so the playlist is the same for every viewer.
func TestRenderHLS_QueryParam(t *testing.T) {
	groups := []ledger.GroupInfo{grp(ledger.NewGroupID(1, 0), 0, 180000, 0)}
	tests := map[string]struct {
		opts renderOpts
		want string
	}{
		"a served init segment": {
			opts: renderOpts{segExt: ".m4s", initURI: "init.m4s", initServed: true, queryParam: "jwt"},
			want: `#EXTM3U
#EXT-X-VERSION:11
#EXT-X-DEFINE:QUERYPARAM="jwt"
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-PLAYLIST-TYPE:EVENT
#EXT-X-MAP:URI="init.m4s?jwt={$jwt}"
#EXTINF:2.000,
e000001-g00000000.m4s?jwt={$jwt}
`,
		},
		// A signed URL would not survive another parameter.
		"an external init segment": {
			opts: renderOpts{segExt: ".m4s", initURI: "https://objects.example/init.m4s?sig=x", queryParam: "jwt"},
			want: `#EXTM3U
#EXT-X-VERSION:11
#EXT-X-DEFINE:QUERYPARAM="jwt"
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-PLAYLIST-TYPE:EVENT
#EXT-X-MAP:URI="https://objects.example/init.m4s?sig=x"
#EXTINF:2.000,
e000001-g00000000.m4s?jwt={$jwt}
`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			body, err := renderHLS(testSchema, groups, tt.opts)

			require.NoError(t, err)
			assert.Equal(t, tt.want, string(body))
		})
	}
}

func TestRenderDASH_QueryParam(t *testing.T) {
	groups := []ledger.GroupInfo{grp(ledger.NewGroupID(1, 0), 0, 180000, 0)}

	body, err := renderDASH(testSchema, groups, renderOpts{segExt: ".m4s", queryParam: "jwt"})

	require.NoError(t, err)
	assert.Contains(t, string(body), `xmlns:up="urn:mpeg:dash:schema:urlparam:2014"`)
	assert.Contains(t, string(body), `<EssentialProperty schemeIdUri="urn:mpeg:dash:urlparam:2014">`)
	assert.Contains(t, string(body), `<up:UrlQueryInfo queryTemplate="$querypart$" useMPDUrlQuery="true"/>`)
	assert.Contains(t, string(body), `<SegmentURL media="e000001-g00000000.m4s"/>`, "the client adds the query; the MPD lists plain URLs")
	assert.NoError(t, xml.Unmarshal(body, new(struct{})), "still well-formed XML, the prefix declared")
}

// Without a query parameter neither manifest changes.
func TestRenderDASH_NoQueryParam(t *testing.T) {
	groups := []ledger.GroupInfo{grp(ledger.NewGroupID(1, 0), 0, 180000, 0)}

	body, err := renderDASH(testSchema, groups, renderOpts{segExt: ".m4s"})

	require.NoError(t, err)
	assert.NotContains(t, string(body), "urlparam")
}
