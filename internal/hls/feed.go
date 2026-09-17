package hls

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/gomoqt/msf"

	"github.com/okdaichi/qumo-ledger/ledger"

	"github.com/qumo-dev/qumo/internal/cmaf"
	"github.com/qumo-dev/qumo/internal/tlsclient"
)

// feedConfig carries the relay subscription parameters, resolved from
// environment by [Run].
type feedConfig struct {
	relayURL  string // relay URL, e.g. "https://host:4433"
	trackPath string // MoQ broadcast path (catalog and media track live under it)
	trackName string // MoQ track name to relay, selected from the catalog
	// caFile, when set, is a PEM cert trusted as the relay's root, overriding
	// the system roots. Empty means verify against the system root store.
	caFile string
	// insecure skips relay TLS verification entirely for a self-signed dev
	// relay. It dominates caFile when both are set — matching crypto/tls, where
	// InsecureSkipVerify short-circuits verification regardless of RootCAs.
	insecure bool

	// liveTimeout is how long the feed waits for a group before deciding the
	// publisher is gone. It is also how stale a manifest may be before the
	// server stops claiming the stream is live — one silence, described once.
	liveTimeout time.Duration
}

// mediaInfo is the selected media track's identity and ledger projection,
// learned from the MSF catalog: schema drives the ledger track, and init is the
// fMP4 initialization segment carried in the catalog's initData.
type mediaInfo struct {
	path   moqt.BroadcastPath
	name   moqt.TrackName
	schema ledger.TrackSchema

	// packager converts the LOC frames arriving over MoQ into the CMAF this
	// egress stores and serves. It also owns the init segment, which it derives
	// from the catalog rather than receiving — so it exists before any media
	// does, and the feed never waits on a publisher to describe itself twice.
	packager *cmaf.Packager
}

// connectRetryInterval is how long connectWithRetry waits between attempts to
// reach the relay and find the publisher's catalog.
const connectRetryInterval = 2 * time.Second

// connectWithRetry calls connect repeatedly until it succeeds or ctx is
// cancelled. The egress and the publisher start as independent processes with
// no ordering guarantee between them — the publisher's catalog track may not
// exist yet, or the relay may not be reachable yet — so neither is treated as
// fatal on startup; only ctx cancellation stops retrying.
func connectWithRetry(ctx context.Context, cfg feedConfig) (*moqt.Session, mediaInfo, error) {
	for attempt := 1; ; attempt++ {
		session, media, err := connect(ctx, cfg)
		if err == nil {
			return session, media, nil
		}
		if ctx.Err() != nil {
			return nil, mediaInfo{}, ctx.Err()
		}

		slog.Warn("hls: waiting for publisher, retrying",
			"attempt", attempt, "path", cfg.trackPath, "err", err)

		select {
		case <-ctx.Done():
			return nil, mediaInfo{}, ctx.Err()
		case <-time.After(connectRetryInterval):
		}
	}
}

// relayTLSConfig builds the client TLS config for dialing the relay. The egress
// verifies the relay's certificate by default: against the system root store
// when caFile is empty, or against a single relay cert when caFile names a PEM.
// insecure opts out of verification entirely for a self-signed dev relay. The
// trust decision itself is shared across qumo's clients via [tlsclient.Apply];
// only the egress-specific base config (TLS 1.3 floor) is owned here.
func relayTLSConfig(caFile string, insecure bool) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS13}
	if err := tlsclient.Apply(tc, caFile, insecure); err != nil {
		return nil, err
	}
	return tc, nil
}

// connect dials the relay, reads the MSF catalog, and returns the selected
// media track's identity, ledger schema, and fMP4 init. The session stays open
// for [feedMedia] to subscribe on.
func connect(ctx context.Context, cfg feedConfig) (*moqt.Session, mediaInfo, error) {
	tc, err := relayTLSConfig(cfg.caFile, cfg.insecure)
	if err != nil {
		return nil, mediaInfo{}, fmt.Errorf("hls: relay TLS config: %w", err)
	}
	session, err := (&moqt.Dialer{TLSConfig: tc}).Dial(ctx, cfg.relayURL, moqt.NewTrackMux(0))
	if err != nil {
		return nil, mediaInfo{}, fmt.Errorf("hls: dial relay: %w", err)
	}

	catalog, err := fetchCatalog(ctx, session, cfg.trackPath)
	if err != nil {
		// not actionable: the close outcome is irrelevant once the catalog read failed.
		_ = session.CloseWithError(moqt.NoError, "catalog fetch failed")
		return nil, mediaInfo{}, err
	}

	track := findTrack(catalog, cfg.trackName)
	if track == nil {
		// not actionable: the close outcome is irrelevant once the track was not found.
		_ = session.CloseWithError(moqt.NoError, "track not found")
		return nil, mediaInfo{}, fmt.Errorf("hls: track %q not in catalog", cfg.trackName)
	}

	// Everything needed to describe the track is stated in the catalog, so the
	// packager — and with it the init segment — is built here, before a single
	// frame arrives. A catalog that cannot describe its track is a publisher
	// that is not ready yet rather than a usable feed, and failing the connect
	// lets connectWithRetry wait with the same backoff it uses for a missing
	// broadcast.
	packager, err := packagerForTrack(catalog, track)
	if err != nil {
		// not actionable: the close outcome is irrelevant once the catalog could not describe the track.
		_ = session.CloseWithError(moqt.NoError, "catalog cannot describe the track")
		return nil, mediaInfo{}, err
	}

	slog.Info("hls: catalog loaded",
		"track", track.Name, "packaging", track.Packaging,
		"codec", track.Codec, "initBytes", len(packager.Init()))

	return session, mediaInfo{
		path:     moqt.BroadcastPath(cfg.trackPath),
		name:     moqt.TrackName(cfg.trackName),
		schema:   trackSchema(track),
		packager: packager,
	}, nil
}

// packagerForTrack builds the CMAF packager a catalog track describes.
func packagerForTrack(c msf.Catalog, t *msf.Track) (*cmaf.Packager, error) {
	if t.Width == nil || t.Height == nil {
		return nil, fmt.Errorf("hls: track %q states no picture size", t.Name)
	}
	return cmaf.NewPackager(cmaf.VideoConfig{
		Codec:  t.Codec,
		Width:  uint16(*t.Width),
		Height: uint16(*t.Height),
		// AVC and HEVC carry their parameter sets out of band; the LOC
		// publisher puts them in the catalog's InitDataList, referenced by
		// the track's initRef.
		Description: initFromTrack(c, t),
	})
}

// fetchCatalog subscribes to the reserved catalog track and parses its first
// group as the MSF catalog.
func fetchCatalog(ctx context.Context, session *moqt.Session, path string) (msf.Catalog, error) {
	cat, err := session.Subscribe(ctx,
		moqt.BroadcastPath(path),
		moqt.TrackName(msf.DefaultCatalogTrackName),
		&moqt.SubscribeConfig{Ordered: true},
	)
	if err != nil {
		return msf.Catalog{}, fmt.Errorf("hls: subscribe catalog: %w", err)
	}
	defer cat.Close()

	gr, err := cat.AcceptGroup(ctx)
	if err != nil {
		return msf.Catalog{}, fmt.Errorf("hls: read catalog group: %w", err)
	}
	data, err := drainRaw(gr, moqt.NewFrame(0))
	if err != nil {
		return msf.Catalog{}, fmt.Errorf("hls: read catalog payload: %w", err)
	}

	catalog, err := msf.ParseCatalog(data)
	if err != nil {
		return msf.Catalog{}, fmt.Errorf("hls: parse catalog: %w", err)
	}
	return catalog, nil
}

// findTrack returns the named track in the catalog, or nil.
func findTrack(c msf.Catalog, name string) *msf.Track {
	for i := range c.Tracks {
		if c.Tracks[i].Name == name {
			return &c.Tracks[i]
		}
	}
	return nil
}

// trackSchema projects an MSF catalog track onto a ledger [ledger.TrackSchema].
//
// The payloads this egress stores are the fragments it packages, not the frames
// it received, so the schema describes those: fragmented MP4 in the packager's
// timescale. Reading a timescale off the wire would describe the wrong thing —
// the media as it arrived, rather than as it is stored.
func trackSchema(t *msf.Track) ledger.TrackSchema {
	s := ledger.TrackSchema{
		Timescale:  cmaf.Timescale,
		TimeSource: ledger.TimeSourceIngest,
		MIME:       "video/mp4",
		Encoding:   "fmp4",
	}
	if t.MimeType != "" {
		s.MIME = t.MimeType
	}
	return s
}

// initFromTrack base64-decodes the fMP4 init data the track's initRef points
// at in the catalog's InitDataList, returning nil when the track carries no
// initRef, the ref is unresolved, or the data is malformed.
func initFromTrack(c msf.Catalog, t *msf.Track) []byte {
	if t.InitRef == "" {
		return nil
	}
	var data string
	for i := range c.InitDataList {
		if c.InitDataList[i].ID == t.InitRef {
			data = c.InitDataList[i].Data
			break
		}
	}
	if data == "" {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		// Treated as absent: the packager rejects a track it cannot describe
		// (an AVC catalog without parameter sets) with a clearer message than a
		// base64 error would give here.
		return nil
	}
	return b
}

// runFeed owns the MoQ session across the lifetime of the process: it feeds
// media through the already-connected session, and on any failure (the
// publisher stops, the connection drops, a subscribe is rejected) reconnects
// via connectWithRetry rather than giving up. This is what lets a publisher
// restart (stop/start in the browser, a crash, a network blip) recover
// without restarting the egress. It returns when ctx is cancelled, and cancels
// ctx itself if the ledger cannot close a finished epoch — a feed that cannot
// reopen has no recovery path.
//
// session/media is the already-connected catalog handoff from [Run]'s initial
// connectWithRetry call, so the very first iteration feeds immediately without
// reconnecting; every iteration after a feed failure reconnects first.
func runFeed(ctx context.Context, cancel context.CancelFunc, session *moqt.Session, media mediaInfo, w *ledger.Writer, live *liveness, cfg feedConfig) {
	for {
		if err := feedMedia(ctx, sessionSubscriber{session}, media, w, live, cfg); err != nil && ctx.Err() == nil {
			slog.Warn("hls: feed ended", "err", err)
		}
		// not actionable: the session already failed or ctx is ending; either
		// way nothing depends on how the close resolves.
		_ = session.CloseWithError(moqt.NoError, "hls feed reconnecting")

		if ctx.Err() != nil {
			return
		}

		// The lifetime ends here, not when the next one begins. Closing the
		// epoch as soon as the feed stops is what makes that visible: the groups
		// just written stop being the newest epoch, so a manifest scoped to the
		// current lifetime empties immediately instead of going on listing a
		// session that has finished until someone else starts one. It also gives
		// whoever connects next its own sequence space — a publisher numbers
		// groups from zero, and appending those to a filled epoch collides with
		// IDs already committed, which a sealed group will not accept.
		if err := w.NewEpoch(ctx); err != nil {
			// A ledger that cannot close an epoch cannot accept a restarted
			// publisher's groups either, so the feed has no recovery path.
			// Stop the egress — cancelling the root context shuts the HTTP
			// server down too — and let a supervisor restart it, rather than
			// staying up serving stale segments.
			slog.Error("hls: cannot close the finished publisher's epoch; stopping the egress", "err", err)
			cancel()
			return
		}
		slog.Info("hls: publisher finished, epoch closed")

		var err error
		session, media, err = connectWithRetry(ctx, cfg)
		if err != nil {
			// ctx was cancelled while waiting to reconnect.
			return
		}
		slog.Info("hls: publisher connected, feeding the new epoch")
	}
}

// The feed talks to two libraries it cannot stand up in a test — a MoQ session
// (which needs a live QUIC relay) and the ledger writer (which needs a store and
// track) — so it depends on small consumer-side interfaces at those seams rather
// than the concrete types. The MoQ seam cannot be [moqt.Session] directly: its
// Subscribe returns the concrete *moqt.TrackReader, which a test cannot build, so
// a thin adapter closes the gap and lets feedMedia's orchestration — skip a group
// that fails to read or package, advance the timeline only on a committed append,
// treat silence as the publisher leaving — run against a fake without a relay.

// mediaSubscriber subscribes to a media track and returns its group source.
type mediaSubscriber interface {
	SubscribeMedia(ctx context.Context, m mediaInfo) (groupFeeder, error)
}

// groupFeeder is a subscribed track: a stream of groups, closed when the feed ends.
type groupFeeder interface {
	AcceptGroup(ctx context.Context) (receivedGroup, error)
	Close() error
}

// receivedGroup is one MoQ group: its producer sequence and its frames, read one
// at a time until io.EOF.
type receivedGroup interface {
	GroupSequence() moqt.GroupSequence
	ReadFrame(*moqt.Frame) error
}

// groupAppender commits a packaged group. The ledger writer satisfies it; a fake
// records what the feed tried to append.
type groupAppender interface {
	AppendGroup(ctx context.Context, meta ledger.GroupInfo, payload []byte) (ledger.GroupInfo, error)
}

// sessionSubscriber adapts [moqt.Session] to [mediaSubscriber].
type sessionSubscriber struct{ sess *moqt.Session }

func (s sessionSubscriber) SubscribeMedia(ctx context.Context, m mediaInfo) (groupFeeder, error) {
	tr, err := s.sess.Subscribe(ctx, m.path, m.name, &moqt.SubscribeConfig{Ordered: true})
	if err != nil {
		return nil, err
	}
	return trackFeeder{tr}, nil
}

// trackFeeder adapts [moqt.TrackReader] to [groupFeeder].
type trackFeeder struct{ tr *moqt.TrackReader }

func (t trackFeeder) AcceptGroup(ctx context.Context) (receivedGroup, error) {
	gr, err := t.tr.AcceptGroup(ctx)
	if err != nil {
		return nil, err
	}
	return moqtGroup{gr}, nil
}

func (t trackFeeder) Close() error { return t.tr.Close() }

// moqtGroup adapts [moqt.GroupReader] to [receivedGroup].
type moqtGroup struct{ gr *moqt.GroupReader }

func (g moqtGroup) GroupSequence() moqt.GroupSequence { return g.gr.GroupSequence() }
func (g moqtGroup) ReadFrame(f *moqt.Frame) error     { return g.gr.ReadFrame(f) }

// Compile-time checks that the adapters and the ledger writer satisfy the seams.
var (
	_ mediaSubscriber = sessionSubscriber{}
	_ groupFeeder     = trackFeeder{}
	_ receivedGroup   = moqtGroup{}
	_ groupAppender   = (*ledger.Writer)(nil)
)

// feedMedia subscribes to the media track and relays each received group into
// the ledger as a sealed group. It blocks until ctx is cancelled or the
// subscription fails.
//
// Each group is a run of LOC frames the publisher sent; drainGroup decodes
// them and the packager turns them into one CMAF (fMP4) fragment, so the
// ledger segment is what the player fetches. MediaTime/Duration are still
// derived (gomoqt v0.15.0 carries no per-frame timestamp); the Timescale now
// comes from the catalog.
func feedMedia(ctx context.Context, sub mediaSubscriber, m mediaInfo, w groupAppender, live *liveness, cfg feedConfig) error {
	src, err := sub.SubscribeMedia(ctx, m)
	if err != nil {
		return fmt.Errorf("hls: subscribe %s: %w", m.name, err)
	}
	defer src.Close()

	slog.Info("hls: subscribed", "path", m.path, "name", m.name)

	frame := moqt.NewFrame(0)
	// mediaTime accumulates the real extents rather than multiplying an ordinal
	// by a fixed step, so a fragment that runs long or short moves the timeline
	// by what it actually contains. anchor is the wall clock at mediaTime zero.
	var mediaTime int64
	var anchor time.Time
	for {
		// Bound the wait for the next group. A publisher that goes away without
		// closing — a browser tab that navigated, a process killed — leaves this
		// blocked on a session that has not timed out yet, and while it blocks
		// the egress neither reconnects nor opens the epoch a restarted
		// publisher needs. Treating silence as the feed ending puts that
		// recovery on a clock the egress controls.
		acceptCtx, cancelAccept := context.WithTimeout(ctx, cfg.liveTimeout)
		gr, err := src.AcceptGroup(acceptCtx)
		cancelAccept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// DeadlineExceeded, not a bare acceptCtx.Err(): cancelAccept just
			// ran, so the context is already Canceled on every error path —
			// only an expired deadline distinguishes a silent publisher from a
			// real accept failure.
			if errors.Is(acceptCtx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("hls: no group for %s, treating the publisher as gone",
					cfg.liveTimeout)
			}
			return fmt.Errorf("hls: accept group: %w", err)
		}

		frames, err := drainGroup(gr, frame)
		if err != nil {
			slog.Warn("hls: read group, skipping", "group", gr.GroupSequence(), "err", err)
			continue
		}
		if len(frames) == 0 {
			continue
		}

		// The ledger's running media time places the fragment, so the fragment
		// and the segment listing cannot disagree about where it belongs — and
		// a group the ledger refuses below advances neither.
		payload, units, err := m.packager.Fragment(frames, uint64(mediaTime))
		if err != nil {
			slog.Warn("hls: package group, skipping", "group", gr.GroupSequence(), "err", err)
			continue
		}

		// The wall clock is read once, when the first group lands, and every
		// group after it is placed by its own media time. Reading the clock per
		// group would stamp arrival jitter onto the timeline instead: two
		// segments a second apart in media would carry whatever gap the network
		// happened to add, and EXT-X-PROGRAM-DATE-TIME would describe the
		// delivery rather than the media it names.
		now := time.Now()
		if anchor.IsZero() {
			anchor = now
		}
		duration := int64(units)
		info := groupInfo(uint64(gr.GroupSequence()), mediaTime, duration, uint64(len(frames)),
			wallclockAt(anchor, mediaTime, m.schema.Timescale))
		if _, err := w.AppendGroup(ctx, info, payload); err != nil {
			// A sealed group is immutable; a duplicate or ordering refusal is
			// skipped rather than stopping the feed.
			slog.Warn("hls: append group, skipping", "group", info.ID, "err", err)
			continue
		}
		// Marked only once the group is committed, so liveness means media a
		// client can actually fetch rather than media that merely arrived.
		live.mark(now)
		mediaTime += duration
	}
}

// drainRaw concatenates a group's frames verbatim. The catalog track carries
// one JSON document rather than media, so its frames are not LOC and are joined
// rather than decoded.
func drainRaw(gr *moqt.GroupReader, frame *moqt.Frame) ([]byte, error) {
	var payload []byte
	for {
		if err := gr.ReadFrame(frame); err != nil {
			if err == io.EOF {
				return payload, nil
			}
			return nil, err
		}
		payload = append(payload, frame.Body()...)
	}
}

// drainGroup reads every frame of a group and decodes each as LOC.
//
// One MoQ frame is one LOC frame, so they are read individually rather than
// concatenated: the group's payload is a run of self-delimiting frames, and
// treating it as one buffer would only mean re-splitting it. io.EOF ends the
// group cleanly.
//
// The first frame of a group is the sync sample. A MoQ group begins at each
// keyframe, so the boundary carries what LOC itself does not state.
func drainGroup(gr receivedGroup, frame *moqt.Frame) ([]cmaf.Frame, error) {
	var frames []cmaf.Frame
	for {
		if err := gr.ReadFrame(frame); err != nil {
			if err == io.EOF {
				return frames, nil
			}
			return nil, err
		}

		timestamp, payload, err := cmaf.DecodeLOC(frame.Body())
		if err != nil {
			return nil, err
		}
		frames = append(frames, cmaf.Frame{
			Timestamp: timestamp,
			Sync:      len(frames) == 0,
			// ReadFrame reuses its buffer, so the payload is copied out before
			// the next read overwrites it.
			Data: bytes.Clone(payload),
		})
	}
}

// groupInfo maps a MoQ group to a ledger [ledger.GroupInfo]. seq is the group's
// producer sequence (its identity, carried in the ID; the writer stamps the
// epoch); mediaTime is where this group starts on the track's timeline; and
// durationUnits is its extent, both in the track's timescale.
//
// Media time is the running sum of the groups actually appended rather than a
// function of the producer's sequence, because MoQ sequences are gappy — a
// dropped group must not leave a hole in the timeline.
func groupInfo(seq uint64, mediaTime, durationUnits int64, objectCount uint64, at time.Time) ledger.GroupInfo {
	return ledger.GroupInfo{
		ID:          ledger.NewGroupID(0, seq),
		MediaTime:   mediaTime,
		Duration:    durationUnits,
		Wallclock:   at.UnixNano(),
		ObjectCount: objectCount,
	}
}

// wallclockAt places a media offset on the wall clock, relative to the anchor
// taken when the feed's first group landed. Seconds and remainder are converted
// separately so a long session cannot overflow the nanosecond multiply.
func wallclockAt(anchor time.Time, mediaTime int64, timescale uint32) time.Time {
	if timescale == 0 {
		return anchor
	}
	ts := int64(timescale)
	seconds := mediaTime / ts
	remainder := mediaTime % ts
	return anchor.
		Add(time.Duration(seconds) * time.Second).
		Add(time.Duration(remainder * int64(time.Second) / ts))
}
