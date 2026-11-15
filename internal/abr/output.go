package abr

import (
	"encoding/base64"
	"fmt"
	"sync"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/gomoqt/msf"
)

// videoAltGroup is the MSF alternate group every video track of an output
// broadcast belongs to: the source and its renditions are alternatives of one
// picture, of which a player decodes one at a time (draft-ietf-moq-msf-01
// §5.2.11).
const videoAltGroup = 1

// Publisher properties of tracks whose publisher states none.
var (
	defaultVideoInfo = moqt.PublishInfo{Priority: 128, Ordered: true, MaxLatency: 2000, Timescale: 1_000_000}
	defaultAudioInfo = moqt.PublishInfo{Priority: 128, Ordered: false, MaxLatency: 2000, Timescale: 1_000_000}
	catalogInfo      = moqt.PublishInfo{Priority: 255, Ordered: true, Timescale: 1000}
)

var (
	_ moqt.TrackHandler      = (*outputBroadcast)(nil)
	_ moqt.TrackInfoProvider = (*outputBroadcast)(nil)
)

// outputBroadcast is the broadcast an ABR pipeline publishes: the source's
// tracks, forwarded, and the renditions, behind one MSF catalog.
type outputBroadcast struct {
	b *msf.Broadcast

	mu   sync.Mutex // serializes catalog changes, guards info
	info map[moqt.TrackName]moqt.PublishInfo
}

func newOutputBroadcast() (*outputBroadcast, error) {
	b, err := msf.NewBroadcast(msf.Catalog{Version: 1})
	if err != nil {
		return nil, fmt.Errorf("abr: output broadcast: %w", err)
	}
	return &outputBroadcast{b: b, info: map[moqt.TrackName]moqt.PublishInfo{}}, nil
}

// ServeTrack serves a subscriber of any track of the broadcast.
func (o *outputBroadcast) ServeTrack(tw *moqt.TrackWriter) {
	o.b.ServeTrack(tw)
}

// TrackInfo answers TRACK_INFO with the publisher properties each track was
// added with.
func (o *outputBroadcast) TrackInfo(name moqt.TrackName) (moqt.PublishInfo, bool) {
	if name == o.b.CatalogTrackName() {
		return catalogInfo, true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	info, ok := o.info[name]
	return info, ok
}

// addTrack adds t to the catalog, or replaces the track of its name, served by
// tr. init, when not empty, is the track's decoder configuration, listed in
// the catalog's InitDataList under the track's name.
func (o *outputBroadcast) addTrack(t msf.Track, init []byte, info moqt.PublishInfo, tr *track) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(init) > 0 {
		catalog := o.b.Catalog()
		entry := msf.InitDataRef{ID: t.Name, Type: "inline", Data: base64.StdEncoding.EncodeToString(init)}
		replaced := false
		for i := range catalog.InitDataList {
			if catalog.InitDataList[i].ID == entry.ID {
				catalog.InitDataList[i] = entry
				replaced = true
			}
		}
		if !replaced {
			catalog.InitDataList = append(catalog.InitDataList, entry)
		}
		if err := o.b.SetCatalog(catalog); err != nil {
			return fmt.Errorf("abr: init data of %s: %w", t.Name, err)
		}
		t.InitRef = entry.ID
	}
	o.info[moqt.TrackName(t.Name)] = info
	if err := o.b.RegisterTrack(t, moqt.TrackHandlerFunc(func(tw *moqt.TrackWriter) {
		tr.serve(tw.Context(), tw)
	})); err != nil {
		return fmt.Errorf("abr: add track %s: %w", t.Name, err)
	}
	return nil
}

// setBitrate states the bitrate of the track named name in the catalog.
func (o *outputBroadcast) setBitrate(name string, bitrate int64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	catalog := o.b.Catalog()
	for i := range catalog.Tracks {
		if catalog.Tracks[i].Name == name {
			catalog.Tracks[i].Bitrate = new(bitrate)
			return o.b.SetCatalog(catalog)
		}
	}
	return fmt.Errorf("abr: no track %s", name)
}

// close ends the broadcast's catalog track for its subscribers.
func (o *outputBroadcast) close() {
	o.b.Close()
}
