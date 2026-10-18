package hls

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/okdaichi/qumo-ledger/ledger"

	"github.com/qumo-dev/qumo/internal/cmaf"
)

// streamHandler serves one track over HTTP as both HLS and DASH. It routes by the
// request URL's base name, so it is mount-point-agnostic — mount it at the
// track's path:
//
//	.../playlist.m3u8 (or any *.m3u8)  the HLS media playlist
//	.../manifest.mpd   (or any *.mpd)   the DASH MPD
//	.../<group-id>.m4s                  one segment — shared by both formats
//	.../init.m4s                        the init segment
//
// The track stores each group as the frames MoQ carried (moq-lite-05, their
// bodies LOC); a segment is that group packaged into a CMAF fragment when it
// is asked for.
//
// A fresh [*ledger.Reader] is opened per request. A Reader is single-consumer,
// and concurrent consumers each opening their own is the documented cheap
// pattern, so the handler does that rather than share one.
type streamHandler struct {
	track       *ledger.Track
	schema      ledger.TrackSchema
	packager    *cmaf.Packager
	window      int
	epochWindow int
	queryParam  string
	logger      *slog.Logger
}

// segExt is the extension segments are named with: each is a CMAF fragment.
const segExt = ".m4s"

// segmentMIME is the media type of the init segment and every fragment.
const segmentMIME = "video/mp4"

// initName is the name, relative to the track, of the init segment.
const initName = "init" + segExt

// streamOptions configures a [streamHandler].
type streamOptions struct {
	// Packager packages the track's groups into CMAF fragments, and holds the
	// initialization segment (ftyp + moov) served at init.m4s and referenced as
	// HLS #EXT-X-MAP and DASH Initialization. It is required.
	Packager *cmaf.Packager

	// Window caps how many of the most recent segments a manifest lists. Zero
	// lists every committed segment.
	//
	// A live track runs indefinitely, so an unwindowed manifest grows without
	// bound: an hour of one-second segments is 3,600 entries re-rendered on
	// every request, and a player re-fetches the whole thing each reload. A
	// window bounds the response and turns the HLS playlist from EVENT (append
	// only, whole history) into a sliding live playlist.
	//
	// It bounds the manifest, not the read: the handler still walks the track to
	// count what precedes the window, because a sliding playlist's
	// EXT-X-MEDIA-SEQUENCE has to be exact. Segments outside the window remain
	// addressable — the ledger deletes nothing, so a client that kept a URL can
	// still fetch it.
	Window int

	// EpochWindow caps how many producer lifetimes a manifest lists, newest
	// first. Zero lists every one; 1 lists only the current session.
	//
	// A producer that restarts opens a new epoch, and everything before it is
	// media from a session that has ended. Left listed, those segments are what
	// a player opening the stream starts on — it plays the previous session
	// through before reaching the current one — and [streamOptions.Window] only
	// clears them once enough new segments have pushed them out.
	//
	// Above 1 keeps recent lifetimes listed, which suits a producer that
	// restarts briefly: a viewer already playing the old epoch reaches the
	// restart across a discontinuity instead of finding its segments gone.
	//
	// Lifetimes are counted per request from the newest group the walk finds,
	// so a restart takes effect on the next manifest rather than when a handler
	// is next built. Earlier epochs stay addressable; they are only absent from
	// the manifest.
	EpochWindow int

	// QueryParam, when set, names a query parameter of the manifest's URL,
	// such as an access token ("jwt"), that clients carry onto every URL the
	// manifest names, so the origin or a CDN can check each request while
	// the manifest stays the same for every viewer, and so cacheable. The
	// HLS playlist declares it with EXT-X-DEFINE:QUERYPARAM (protocol
	// version 11) and appends name={$name} to each URI; the DASH MPD carries
	// UrlQueryInfo (ISO/IEC 23009-1 Annex I), which makes the client repeat
	// the MPD URL's query on segment requests.
	//
	// An HLS client refuses a playlist declaring a QUERYPARAM its URL lacks,
	// so set it only where every manifest URL carries the parameter. It must
	// be an HLS variable name (letters, digits, '-' and '_'); the caller
	// validates it.
	QueryParam string
}

// newStreamHandler builds a [streamHandler] over track, whose groups opts.Packager
// packages. The track's media time is the packager's: [cmaf.Timescale].
func newStreamHandler(track *ledger.Track, opts streamOptions) (*streamHandler, error) {
	if track == nil {
		return nil, errors.New("hls: nil track")
	}
	if opts.Packager == nil {
		return nil, errors.New("hls: no packager")
	}
	schema := track.Root().TrackSchema
	if schema.Timescale != cmaf.Timescale {
		return nil, fmt.Errorf("hls: track timescale %d is not the packager's %d", schema.Timescale, cmaf.Timescale)
	}
	return &streamHandler{
		track:       track,
		schema:      schema,
		packager:    opts.Packager,
		window:      opts.Window,
		epochWindow: opts.EpochWindow,
		queryParam:  opts.QueryParam,
		logger:      slog.Default(),
	}, nil
}

// ServeHTTP routes a request to the playlist, manifest, segment, or init handler.
func (h *streamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	base := path.Base(r.URL.Path)
	switch {
	case strings.HasSuffix(base, ".m3u8"):
		h.serveHLS(w, r)
	case strings.HasSuffix(base, ".mpd"):
		h.serveDASH(w, r)
	case base == initName:
		h.serveInit(w)
	default:
		h.serveSegment(w, r)
	}
}

// internalError answers a generic 500 and logs what actually failed. The
// errors on these paths carry object keys and store paths — detail a client
// has no use for and should not see — so the log is where it survives.
//
// A canceled context is the client leaving (a seek, a variant switch) and is
// not logged: routine player churn must not flood the record of real failures.
func (h *streamHandler) internalError(w http.ResponseWriter, r *http.Request, op string, err error) {
	if !errors.Is(err, context.Canceled) {
		// The path, not the URL: the query can carry a viewer's token.
		h.logger.Error("hls: "+op, "method", r.Method, "path", r.URL.Path, "err", err)
	}
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}

// serveHLS renders and writes the HLS media playlist.
func (h *streamHandler) serveHLS(w http.ResponseWriter, r *http.Request) {
	window, err := h.gather(r.Context())
	if err != nil {
		h.internalError(w, r, "gather playlist window", err)
		return
	}
	body, err := renderHLS(h.schema, window.groups, h.renderOpts(window))
	if err != nil {
		h.internalError(w, r, "render playlist", err)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	_, _ = w.Write(body) // not actionable: once the body write fails the client is gone
}

// serveDASH renders and writes the DASH MPD.
func (h *streamHandler) serveDASH(w http.ResponseWriter, r *http.Request) {
	window, err := h.gather(r.Context())
	if err != nil {
		h.internalError(w, r, "gather manifest window", err)
		return
	}
	body, err := renderDASH(h.schema, window.groups, h.renderOpts(window))
	if err != nil {
		h.internalError(w, r, "render manifest", err)
		return
	}
	w.Header().Set("Content-Type", "application/dash+xml")
	_, _ = w.Write(body) // not actionable: once the body write fails the client is gone
}

// serveInit writes the init segment.
func (h *streamHandler) serveInit(w http.ResponseWriter) {
	init := h.packager.Init()
	w.Header().Set("Content-Type", segmentMIME)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(init)))
	//nolint:gosec // G705: CMAF init segment, typed video/mp4 and not sniffed.
	_, _ = w.Write(init) // not actionable: once the body write fails the client is gone
}

// serveSegment reads a group by its GroupID out of the ledger and packages it
// into the segment. One handler serves both HLS and DASH, since both address
// segments by GroupID.
func (h *streamHandler) serveSegment(w http.ResponseWriter, r *http.Request) {
	id, ok := parseSegmentID(path.Base(r.URL.Path))
	if !ok {
		http.NotFound(w, r)
		return
	}

	reader, err := h.track.Reader(r.Context())
	if err != nil {
		h.internalError(w, r, "open reader", err)
		return
	}

	group, err := reader.Lookup(r.Context(), id)
	if err != nil {
		// No committed group for the id: the segment was never written, or the
		// client is asking ahead of the tip. A missing epoch log is the same
		// verdict — the ledger treats it as a creation that did not finish, so
		// no group in that epoch can exist. Anything else is a failure to
		// answer rather than an absence, and a 404 here would tell a player to
		// prune a segment that exists.
		if errors.Is(err, ledger.ErrGroupNotFound) || errors.Is(err, ledger.ErrEpochNotFound) {
			http.NotFound(w, r)
			return
		}
		h.internalError(w, r, "lookup segment", err)
		return
	}

	payload, err := reader.ReadGroup(r.Context(), group.ObjectKey)
	if err != nil {
		h.internalError(w, r, "read segment", err)
		return
	}
	data, err := h.fragment(group, payload)
	if err != nil {
		h.internalError(w, r, "package segment", err)
		return
	}

	w.Header().Set("Content-Type", segmentMIME)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	//nolint:gosec // G705: a CMAF fragment of stored media, typed video/mp4 and not sniffed, not request input reflected back.
	_, _ = w.Write(data) // not actionable: once the body write fails the client is gone
}

// fragment packages a stored group, whose frames payload holds, into the CMAF
// fragment the manifests list it as: placed at its MediaTime and covering its
// Duration. The last frame's duration is what the writer stated, the part of
// Duration its frames' timestamps do not span, so the fragment's extent is
// the EXTINF exactly.
func (h *streamHandler) fragment(g ledger.GroupInfo, payload []byte) ([]byte, error) {
	var frames []cmaf.Frame
	for f, err := range allFrames(payload) {
		if err != nil {
			return nil, err
		}
		ts, data, err := cmaf.DecodeLOC(f.Body)
		if err != nil {
			return nil, fmt.Errorf("frame %d: %w", len(frames), err)
		}
		frames = append(frames, cmaf.Frame{
			Timestamp: ts,
			Sync:      len(frames) == 0,
			Data:      data,
		})
	}
	if len(frames) == 0 {
		return nil, fmt.Errorf("group %s has no frames", g.ID)
	}
	span := int64(frames[len(frames)-1].Timestamp - frames[0].Timestamp)
	last := g.Duration - span
	if last <= 0 || last > math.MaxUint32 {
		return nil, fmt.Errorf("group %s: duration %d does not cover its frames' %d", g.ID, g.Duration, span)
	}
	// The producer's sequence numbers the fragment: it is what orders the
	// track's fragments, and restarts with an epoch, as a new session does.
	data, _, err := h.packager.Fragment(uint32(g.ID.Sequence())+1, frames, uint64(g.MediaTime), uint32(last))
	return data, err
}

// manifestWindow is what the renderers need about a track: the groups to list,
// and what the groups before them imply for the numbering a sliding manifest has
// to carry.
type manifestWindow struct {
	// groups are the segments to list, in commit order.
	groups []ledger.GroupInfo

	// dropped is how many segments preceded the window — the first listed
	// segment's media sequence.
	dropped uint64

	// discontinuities is how many epoch changes preceded the window: the
	// discontinuity sequence the first listed segment continues from.
	discontinuities uint64

	// precedingEpoch is the epoch of the segment immediately before the window,
	// or zero when the window starts at the track's beginning. A window that
	// opens on a new epoch has a timeline reset at its very first segment, and
	// nothing in the listed groups themselves reveals it.
	precedingEpoch uint64
}

// gather opens a Reader, rewinds to the start, and drains every committed group
// in commit order. It is the shared input to both renderers.
//
// With a window configured it keeps only the newest Window groups, in a ring,
// and accumulates what the dropped groups imply for the manifest's numbering. A
// sliding playlist must state its media and discontinuity sequences exactly and
// consistently across reloads, which is what the full walk buys — the output is
// short but the counts are not derivable from it. The walk reads manifests, not
// segment payloads.
func (h *streamHandler) gather(ctx context.Context) (manifestWindow, error) {
	reader, err := h.track.Reader(ctx)
	if err != nil {
		return manifestWindow{}, err
	}
	reader.SeekStart()

	var out manifestWindow

	// ring holds the last h.window entries when windowing; next is where the
	// following one lands, wrapping once the ring is full. Unwindowed, every
	// entry is kept and next is unused.
	var ring []streamEntry
	var next int
	if h.window > 0 {
		ring = make([]streamEntry, 0, h.window)
	}

	var prevEpoch uint64
	var seen uint64

	for {
		g, err := reader.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return manifestWindow{}, err
		}

		epoch := g.ID.Epoch()
		e := streamEntry{info: g, discontinuity: seen > 0 && epoch != prevEpoch}
		prevEpoch = epoch
		seen++

		if h.window <= 0 || len(ring) < h.window {
			ring = append(ring, e)
			continue
		}

		// The evicted entry is now behind the window: it becomes what precedes
		// the first listed segment, and its own discontinuity has rolled off.
		out.evict(ring[next])

		ring[next] = e
		next = (next + 1) % h.window
	}

	// Unroll the ring back into commit order: once full, the oldest entry sits
	// at the write cursor.
	ordered := ring
	if h.window > 0 && len(ring) == h.window {
		ordered = slices.Concat(ring[next:], ring[:next])
	}

	// Lifetimes past the epoch window are sessions that have already ended, so
	// they leave the manifest the same way a windowed-out segment does: still
	// addressable, just not listed, and counted as preceding the first one that
	// is.
	for range h.epochsToDrop(ordered) {
		out.evict(ordered[0])
		ordered = ordered[1:]
	}

	out.groups = make([]ledger.GroupInfo, len(ordered))
	for i, e := range ordered {
		out.groups[i] = e.info
	}
	return out, nil
}

// epochsToDrop counts the leading entries that fall outside the epoch window —
// those belonging to a lifetime older than the newest h.epochWindow.
//
// Lifetimes are counted backwards from the newest group rather than by
// subtracting from the latest epoch number, so the answer depends only on what
// is actually listed. Epochs the segment window has already trimmed away are
// not lifetimes this manifest still shows, and counting them would evict a
// session that is genuinely on screen.
func (h *streamHandler) epochsToDrop(ordered []streamEntry) int {
	if h.epochWindow <= 0 || len(ordered) == 0 {
		return 0
	}

	lifetimes := 1
	epoch := ordered[len(ordered)-1].info.ID.Epoch()
	for i := len(ordered) - 1; i >= 0; i-- {
		if ordered[i].info.ID.Epoch() == epoch {
			continue
		}
		epoch = ordered[i].info.ID.Epoch()
		lifetimes++
		if lifetimes > h.epochWindow {
			// Entry i opens the first lifetime past the window, so it and
			// everything before it goes.
			return i + 1
		}
	}
	return 0
}

// streamEntry is a group together with what its position implies for the manifest. A
// discontinuity belongs to the segment that opens a new epoch, so it rolls off
// when that segment does — carrying the flag alongside the group beats
// recomputing it from a window that no longer contains the boundary.
type streamEntry struct {
	info          ledger.GroupInfo
	discontinuity bool
}

// evict folds a group that is no longer listed into the counts a sliding
// manifest states: it becomes what precedes the window, its place in the media
// sequence is spent, and any timeline reset it carried is now behind the
// manifest rather than inside it.
func (w *manifestWindow) evict(e streamEntry) {
	w.precedingEpoch = e.info.ID.Epoch()
	w.dropped++
	if e.discontinuity {
		w.discontinuities++
	}
}

// renderOpts is the renderers' input projected from the handler's state and the
// window it just gathered.
func (h *streamHandler) renderOpts(window manifestWindow) renderOpts {
	return renderOpts{
		queryParam:            h.queryParam,
		sliding:               h.window > 0,
		mediaSequence:         window.dropped,
		discontinuitySequence: window.discontinuities,
		precedingEpoch:        window.precedingEpoch,
	}
}

// parseSegmentID reads a GroupID off a segment URL's base name by stripping the
// segment extension. The second result is false when the name is not a group id.
func parseSegmentID(base string) (ledger.GroupID, bool) {
	id, err := ledger.ParseGroupID(strings.TrimSuffix(base, segExt))
	if err != nil || id == 0 {
		return 0, false
	}
	return id, true
}
