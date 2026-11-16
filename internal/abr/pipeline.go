package abr

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/gomoqt/msf"
	"golang.org/x/sync/errgroup"

	"github.com/qumo-dev/qumo/internal/cmaf"
	"github.com/qumo-dev/qumo/internal/ingest"
)

// Config is what an ABR pipeline makes of a source broadcast.
type Config struct {
	// Ladder lists the renditions to encode, tallest first. Only the rungs
	// shorter than the source are encoded.
	Ladder []Rung
	// Encoder is how they are encoded.
	Encoder EncoderConfig
}

// encodeQueue is how many source frames may wait for the encoder. Past it the
// encoder is behind, and frames are dropped up to the next group, where the
// encoder can pick up again from a keyframe.
const encodeQueue = 120

// pipeline turns one source broadcast into its output broadcast.
type pipeline struct {
	sess *moqt.Session
	src  moqt.BroadcastPath
	cfg  Config
	out  *outputBroadcast

	// started names the source tracks being handled, and transcoding
	// reports that one of them is being transcoded. Only the catalog
	// follower touches them.
	started     map[string]bool
	transcoding bool
}

// run runs the pipeline for src until ctx ends or the source broadcast ends,
// publishing the output broadcast at dst on mux while it runs.
func run(ctx context.Context, sess *moqt.Session, mux *moqt.TrackMux, src, dst moqt.BroadcastPath, cfg Config) error {
	out, err := newOutputBroadcast()
	if err != nil {
		return err
	}
	defer out.close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	mux.Publish(ctx, dst, out)

	p := &pipeline{sess: sess, src: src, cfg: cfg, out: out, started: map[string]bool{}}
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return p.followCatalog(ctx, g) })
	return g.Wait()
}

// followCatalog reads the source's catalog, and each update of it, and starts
// handling every track as it first appears in it. It returns when the source
// ends its catalog track.
func (p *pipeline) followCatalog(ctx context.Context, g *errgroup.Group) error {
	tr, err := p.sess.Subscribe(ctx, p.src, msf.DefaultCatalogTrackName, &moqt.SubscribeConfig{Ordered: true})
	if err != nil {
		return fmt.Errorf("abr: subscribe catalog: %w", err)
	}
	defer tr.Close() // not actionable: the pipeline is ending
	f := moqt.NewFrame(0)
	for {
		gr, err := tr.AcceptGroup(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil // the source ended
			}
			return fmt.Errorf("abr: read catalog: %w", err)
		}
		if err := gr.ReadFrame(f); err != nil {
			gr.CancelRead(moqt.InternalGroupErrorCode)
			continue
		}
		gr.CancelRead(moqt.InternalGroupErrorCode) // one frame per catalog group
		catalog, err := msf.ParseCatalog(f.Body())
		if err != nil {
			slog.Warn("abr: source catalog does not parse", "broadcast", p.src, "error", err)
			continue
		}
		for _, t := range catalog.Tracks {
			if t.Namespace != "" && t.Namespace != string(p.src) {
				continue // a track of another broadcast
			}
			if p.started[t.Name] {
				continue
			}
			p.started[t.Name] = true
			init, err := initData(catalog, t)
			if err != nil {
				slog.Warn("abr: source track has unusable init data; forwarding it without", "broadcast", p.src, "track", t.Name, "error", err)
			}
			if t.Role == msf.RoleVideo && !p.transcoding && isAVC(t.Codec) && init != nil {
				p.transcoding = true
				g.Go(func() error { return p.transcode(ctx, t, init) })
			} else {
				g.Go(func() error { return p.forward(ctx, t, init) })
			}
		}
	}
}

func isAVC(codec string) bool {
	return strings.HasPrefix(codec, "avc1.")
}

// initData returns the decoder configuration the catalog lists for t, or nil
// when it lists none.
func initData(c msf.Catalog, t msf.Track) ([]byte, error) {
	if t.InitRef == "" {
		return nil, nil
	}
	for _, e := range c.InitDataList {
		if e.ID == t.InitRef {
			if e.Type != "" && e.Type != "inline" {
				return nil, fmt.Errorf("init data %q is %q, not inline", e.ID, e.Type)
			}
			return base64.StdEncoding.DecodeString(e.Data)
		}
	}
	return nil, fmt.Errorf("no init data %q", t.InitRef)
}

// publishInfo returns the source track's publisher properties, or def when
// the source does not state them.
func (p *pipeline) publishInfo(ctx context.Context, name string, def moqt.PublishInfo) moqt.PublishInfo {
	info, err := p.sess.TrackInfo(ctx, p.src, moqt.TrackName(name))
	if err != nil || info == nil {
		return def
	}
	return *info
}

// outputTrack returns the catalog entry of a forwarded source track: the
// source's, in the output broadcast's namespace.
func outputTrack(t msf.Track) msf.Track {
	t.Namespace = ""
	t.InitRef = ""
	return t
}

// forward republishes a source track as it is, group for group.
func (p *pipeline) forward(ctx context.Context, t msf.Track, init []byte) error {
	def := defaultAudioInfo
	if t.Role == msf.RoleVideo {
		def = defaultVideoInfo
	}
	out := newTrack()
	defer out.close()
	if err := p.out.addTrack(outputTrack(t), init, p.publishInfo(ctx, t.Name, def), out); err != nil {
		return err
	}
	return p.readTrack(ctx, t.Name, func(seq moqt.GroupSequence) bool {
		return out.openGroup(seq)
	}, func(_ moqt.GroupSequence, _ int, f *moqt.Frame) {
		out.writeFrame(f)
	}, out.endGroup)
}

// readTrack subscribes to the source track name and reads its groups in
// sequence order, skipping any that arrive after a later one. For each group
// it calls open, then frame for each frame (a copy the callee may keep), then
// end. open may refuse the group, which is then skipped.
func (p *pipeline) readTrack(ctx context.Context, name string,
	open func(moqt.GroupSequence) bool, frame func(moqt.GroupSequence, int, *moqt.Frame), end func()) error {
	tr, err := p.sess.Subscribe(ctx, p.src, moqt.TrackName(name), &moqt.SubscribeConfig{Ordered: true})
	if err != nil {
		return fmt.Errorf("abr: subscribe %s: %w", name, err)
	}
	defer tr.Close() // not actionable: the pipeline is ending

	// The sequence of the last group read; read reports there has been one,
	// so a first group numbered 0 is read rather than taken as seen.
	var (
		last moqt.GroupSequence
		read bool
	)
	buf := moqt.NewFrame(0)
	for {
		gr, err := tr.AcceptGroup(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil // the source ended the track
			}
			return fmt.Errorf("abr: read %s: %w", name, err)
		}
		seq := gr.GroupSequence()
		if (read && seq <= last) || !open(seq) {
			gr.CancelRead(moqt.InternalGroupErrorCode)
			continue
		}
		last, read = seq, true
		for i := 0; ; i++ {
			if err := gr.ReadFrame(buf); err != nil {
				break // the group ended, or was reset
			}
			frame(seq, i, buf.Clone())
		}
		end()
	}
}

// transcode forwards the source video track and encodes it into the
// renditions of the ladder shorter than it.
func (p *pipeline) transcode(ctx context.Context, t msf.Track, init []byte) error {
	cfg, err := ingest.ParseAVCConfig(append([]byte{0x17, 0, 0, 0, 0}, init...))
	if err != nil {
		return fmt.Errorf("abr: source decoder configuration: %w", err)
	}
	width, height := cfg.Width, cfg.Height
	if t.Width != nil && t.Height != nil && *t.Width > 0 && *t.Height > 0 {
		width, height = int(*t.Width), int(*t.Height)
	}
	info := p.publishInfo(ctx, t.Name, defaultVideoInfo)

	// The source itself is the top rendition.
	src := outputTrack(t)
	src.AltGroup = new(int64(videoAltGroup))
	src.Width, src.Height = new(int64(width)), new(int64(height))
	if src.Label == "" {
		src.Label = fmt.Sprintf("%dp (source)", height)
	}
	srcOut := newTrack()
	defer srcOut.close()
	if err := p.out.addTrack(src, init, info, srcOut); err != nil {
		return err
	}

	var outputs []output
	for _, r := range below(p.cfg.Ladder, height) {
		w, h := r.Size(width, height)
		outputs = append(outputs, output{Rung: r, Width: w, Height: h})
	}
	if len(outputs) == 0 {
		slog.Info("abr: source is no taller than any rung; forwarding it alone", "broadcast", p.src, "height", height)
	}
	r := &renditions{out: p.out, outputs: outputs, info: info, tracks: make([]*track, len(outputs)), configured: make([]bool, len(outputs))}
	for i := range r.tracks {
		r.tracks[i] = newTrack()
		defer r.tracks[i].close()
	}

	// One ffmpeg decodes the source once for every rendition. Windows cannot
	// hand a process the extra pipes that takes, so there each rendition has
	// an ffmpeg of its own.
	groups := [][]int{}
	if runtime.GOOS == "windows" {
		for i := range outputs {
			groups = append(groups, []int{i})
		}
	} else if len(outputs) > 0 {
		all := make([]int, len(outputs))
		for i := range all {
			all[i] = i
		}
		groups = append(groups, all)
	}
	sc := sourceConfig{Codec: t.Codec, Width: cfg.Width, Height: cfg.Height, Description: init}
	encoders, err := startEncoders(ctx, p.cfg.Encoder, sc, outputs, groups, r)
	if err != nil {
		return err
	}

	// The encoders are fed from a queue, so a slow encoder never holds up the
	// source track's own forwarding.
	queue := make(chan sourceFrame, encodeQueue)
	fed := make(chan error, 1)
	go func() {
		fed <- feed(queue, encoders)
	}()

	rate := newBitrateMeter()
	dropping := false
	err = p.readTrack(ctx, t.Name, srcOut.openGroup, func(seq moqt.GroupSequence, i int, f *moqt.Frame) {
		srcOut.writeFrame(f)
		ts, data, err := cmaf.DecodeLOC(f.Body())
		if err != nil {
			slog.Warn("abr: source frame is not LOC", "broadcast", p.src, "error", err)
			return
		}
		if bps, ok := rate.add(ts, len(data), i == 0); ok {
			if err := p.out.setBitrate(src.Name, bps); err != nil {
				slog.Warn("abr: state source bitrate", "error", err)
			}
		}
		if len(encoders) == 0 {
			return
		}
		if dropping && i != 0 {
			return
		}
		select {
		case queue <- sourceFrame{frameInfo: frameInfo{Group: seq, First: i == 0, Timestamp: ts, WireTimestamp: f.Timestamp}, Data: data}:
			dropping = false
		default:
			if !dropping {
				slog.Warn("abr: encoder is behind; dropping source frames up to the next group", "broadcast", p.src)
			}
			dropping = true
		}
	}, srcOut.endGroup)
	close(queue)
	if ferr := <-fed; ferr != nil && err == nil && ctx.Err() == nil {
		err = ferr
	}
	return err
}

// startEncoders starts an encoder for each group of output indexes. If one
// cannot start, those already started are closed: their ffmpeg is stopped
// and waited for, rather than left to the context and never reaped.
func startEncoders(ctx context.Context, cfg EncoderConfig, src sourceConfig, outputs []output, groups [][]int, r *renditions) ([]*encoder, error) {
	var encoders []*encoder
	for _, idx := range groups {
		outs := make([]output, len(idx))
		for k, i := range idx {
			outs[k] = outputs[i]
		}
		enc, err := newEncoder(cfg, src, outs, remap{r, idx})
		if err == nil {
			err = enc.start(ctx)
		}
		if err != nil {
			for _, started := range encoders {
				_ = started.close() // not actionable: the start failed either way
			}
			return nil, err
		}
		encoders = append(encoders, enc)
	}
	return encoders, nil
}

// feed feeds the frames of queue to every encoder, then closes them.
func feed(queue <-chan sourceFrame, encoders []*encoder) error {
	var errs []error
	failed := false
	for f := range queue {
		if failed {
			continue // drain, so the reader never blocks
		}
		for _, enc := range encoders {
			if err := enc.encode(f); err != nil {
				errs = append(errs, err)
				failed = true
			}
		}
	}
	for _, enc := range encoders {
		if err := enc.close(); err != nil && !failed {
			errs = append(errs, fmt.Errorf("abr: ffmpeg: %w", err))
		}
	}
	return errors.Join(errs...)
}

// remap is the sink of an encoder producing a subset of the renditions: it
// maps the encoder's output indexes onto the renditions'.
type remap struct {
	r   *renditions
	idx []int
}

func (m remap) Configure(i int, record []byte)     { m.r.Configure(m.idx[i], record) }
func (m remap) WriteFrame(i int, f renditionFrame) { m.r.WriteFrame(m.idx[i], f) }

// renditions publishes what the encoders output, one track per rendition.
type renditions struct {
	out     *outputBroadcast
	outputs []output
	info    moqt.PublishInfo
	tracks  []*track

	mu         sync.Mutex
	configured []bool
}

// Configure adds rendition i's track to the catalog with its decoder
// configuration, or updates the configuration of a track already added.
func (r *renditions) Configure(i int, record []byte) {
	cfg, err := ingest.ParseAVCConfig(append([]byte{0x17, 0, 0, 0, 0}, record...))
	if err != nil {
		slog.Warn("abr: rendition decoder configuration does not parse", "rendition", r.outputs[i].Rung.TrackName(), "error", err)
		return
	}
	o := r.outputs[i]
	t := msf.Track{
		Name:      o.Rung.TrackName(),
		Packaging: msf.PackagingLOC,
		Role:      msf.RoleVideo,
		IsLive:    new(true),
		Label:     fmt.Sprintf("%dp", o.Height),
		AltGroup:  new(int64(videoAltGroup)),
		Codec:     cfg.CodecString(),
		Width:     new(int64(o.Width)),
		Height:    new(int64(o.Height)),
		Bitrate:   new(o.Rung.Bitrate),
	}
	if err := r.out.addTrack(t, record, r.info, r.tracks[i]); err != nil {
		slog.Warn("abr: add rendition", "rendition", t.Name, "error", err)
		return
	}
	r.mu.Lock()
	r.configured[i] = true
	r.mu.Unlock()
}

// WriteFrame adds a frame to rendition i's track, opening a group wherever the
// source opened one, so the rendition's groups are numbered as the source's
// and start at the same frames.
func (r *renditions) WriteFrame(i int, f renditionFrame) {
	r.mu.Lock()
	configured := r.configured[i]
	r.mu.Unlock()
	if !configured {
		return
	}
	tr := r.tracks[i]
	if f.First {
		if !f.Keyframe {
			// Not a decode point: a subscriber joining here could not
			// decode it, so the group continues the previous one.
			slog.Warn("abr: rendition frame opening a group is not a keyframe", "rendition", r.outputs[i].Rung.TrackName(), "group", f.Group)
		} else {
			tr.openGroup(f.Group)
		}
	}
	loc := cmaf.EncodeLOC(f.Timestamp, f.Data)
	mf := moqt.NewFrame(len(loc))
	mf.Timestamp = f.WireTimestamp
	_, _ = mf.Write(loc) // not actionable: a Frame grows to fit
	tr.writeFrame(mf)    // refused before the first group opens: frames that cannot be decoded
}
