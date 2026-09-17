//go:build integration

package integration

import (
	"context"
	"fmt"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/gomoqt/msf"
)

const defaultGroupTimeout = 5 * time.Second

// Resource ceilings: the collector subscribes to a relay and must not trust it
// to be well-behaved. A malformed/buggy relay could otherwise grow memory
// without bound (a giant catalog) or loop forever (a stream of tiny frames).
const (
	maxCatalogBytes   = 1 << 20 // 1 MiB; a real MSF catalog is a few KiB.
	maxFramesPerTrack = 1_000_000
)

// Collector subscribes to a broadcast over MoQT, drains its catalog and media
// tracks, and returns an [Observation]. It is the live peer of the pure
// [Evaluate] gate; the gate's logic is unit-tested separately against synthetic
// fixtures (see gate_test.go). The collector itself is exercised by the M5
// in-process relay integration test.
type Collector struct {
	Dialer *moqt.Dialer
	URL    string             // relay URL (e.g. https://host:port)
	Path   moqt.BroadcastPath // broadcast path to subscribe to
	// MaxGroups bounds how many groups to drain per media track before stopping.
	// <=0 drains until the context is cancelled or the group timeout elapses.
	MaxGroups int
	// GroupTimeout caps how long to wait for the next group before treating the
	// stream as ended. <=0 uses defaultGroupTimeout (5s).
	GroupTimeout time.Duration
}

func (c *Collector) groupTimeout() time.Duration {
	if c.GroupTimeout > 0 {
		return c.GroupTimeout
	}
	return defaultGroupTimeout
}

// Collect dials the relay, fetches the catalog, and drains each media track. It
// always returns an [Observation] (possibly partially filled) even on error.
func (c *Collector) Collect(ctx context.Context) (*Observation, error) {
	obs := &Observation{Tracks: map[string]*TrackObs{}}

	sess, err := c.Dialer.Dial(ctx, c.URL, moqt.NewTrackMux(0))
	if err != nil {
		return obs, fmt.Errorf("interop: dial %s: %w", c.URL, err)
	}
	defer sess.CloseWithError(moqt.NoError, "interop done")

	if err := c.collectCatalog(ctx, sess, obs); err != nil {
		obs.CatalogError = err
		return obs, nil
	}
	obs.CatalogFetched = true

	for _, name := range obs.Order {
		t := obs.Tracks[name]
		if t.Role != string(msf.RoleVideo) && t.Role != string(msf.RoleAudio) {
			continue
		}
		c.collectMedia(ctx, sess, t)
	}
	return obs, nil
}

// collectCatalog subscribes to the "catalog" track, reads the first group, and
// parses the MSF catalog into the observation's track map.
func (c *Collector) collectCatalog(ctx context.Context, sess *moqt.Session, obs *Observation) error {
	tr, err := sess.Subscribe(ctx, c.Path, moqt.TrackName("catalog"), nil)
	if err != nil {
		return fmt.Errorf("subscribe catalog: %w", err)
	}
	defer tr.Close()

	// Bound the catalog wait: a publisher that never sends a sequence header
	// (or a relay that does not serve subscribers) must not hang the collector.
	cctx, cancel := context.WithTimeout(ctx, c.groupTimeout())
	defer cancel()
	gr, err := tr.AcceptGroup(cctx)
	if err != nil {
		return fmt.Errorf("read catalog group: %w", err)
	}
	buf := moqt.NewFrame(4096)
	var raw []byte
	for frame := range gr.Frames(buf) {
		if len(raw)+len(frame.Body()) > maxCatalogBytes {
			return fmt.Errorf("catalog exceeds %d-byte cap", maxCatalogBytes)
		}
		raw = append(raw, frame.Body()...)
	}

	cat, err := msf.ParseCatalog(raw)
	if err != nil {
		return fmt.Errorf("parse catalog: %w", err)
	}
	for i := range cat.Tracks {
		t := &cat.Tracks[i]
		to := &TrackObs{
			Name:     t.Name,
			Role:     string(t.Role),
			Codec:    t.Codec,
			InitData: resolveInitData(cat, t.InitRef),
		}
		if t.Width != nil {
			to.Width = int(*t.Width)
		}
		if t.Height != nil {
			to.Height = int(*t.Height)
		}
		obs.Tracks[t.Name] = to
		obs.Order = append(obs.Order, t.Name)
	}
	return nil
}

// resolveInitData looks up the base64 init data an InitRef points at in the
// catalog's InitDataList (draft-ietf-moq-msf-01 §5.1.7/§5.2.13). Returns ""
// when ref is empty or unresolved.
func resolveInitData(cat msf.Catalog, ref string) string {
	if ref == "" {
		return ""
	}
	for i := range cat.InitDataList {
		if cat.InitDataList[i].ID == ref {
			return cat.InitDataList[i].Data
		}
	}
	return ""
}

// collectMedia subscribes to one media track and drains up to MaxGroups groups
// into the track observation.
func (c *Collector) collectMedia(ctx context.Context, sess *moqt.Session, t *TrackObs) {
	tr, err := sess.Subscribe(ctx, c.Path, moqt.TrackName(t.Name), nil)
	if err != nil {
		t.ReadError = err
		return
	}
	defer tr.Close()

	isVideo := t.Role == string(msf.RoleVideo)
	buf := moqt.NewFrame(1500)
	timeout := c.groupTimeout()

	for (c.MaxGroups <= 0 || t.GroupCount < c.MaxGroups) && len(t.Frames) < maxFramesPerTrack {
		if ctx.Err() != nil {
			return
		}
		gctx, cancel := context.WithTimeout(ctx, timeout)
		gr, err := tr.AcceptGroup(gctx)
		cancel()
		if err != nil {
			// Timeout / cancelled / stream ended: normal termination.
			return
		}
		t.GroupCount++
		for frame := range gr.Frames(buf) {
			pts, data, derr := decodeMediaFrame(frame.Body())
			if derr != nil {
				continue
			}
			fo := FrameObs{PTSUS: pts, Bytes: len(data)}
			if isVideo {
				fo.IsKeyframe = isAVCCKeyframe(data)
			}
			t.Frames = append(t.Frames, fo)
		}
	}
}
