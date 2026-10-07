package funnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/okdaichi/qumo-ledger/ingest"
	"github.com/okdaichi/qumo-ledger/ledger"
	"github.com/okdaichi/qumo-ledger/ledger/store"
	"github.com/qumo-dev/gomoqt/moqt"
)

// rootObject is the object every ledger track keeps at its root, as
// <track>/root.manifest.
const rootObject = "root.manifest"

// restore announces the tracks objects already holds that ingest recorded,
// each with its latest record, so a subscriber reaches them before any
// contributor announces again. It returns how many tracks it restored. A
// store that cannot list its keys restores nothing.
func restore(ctx context.Context, objects store.Store, out *egress) (int, error) {
	lister, ok := objects.(store.Lister)
	if !ok {
		return 0, nil
	}
	restored := 0
	for key, err := range lister.List(ctx, "") {
		if err != nil {
			return restored, fmt.Errorf("funnel: list the ledger store: %w", err)
		}
		dir, ok := strings.CutSuffix(key, "/"+rootObject)
		if !ok {
			continue
		}
		ok, err := restoreTrack(ctx, objects, ledger.TrackPath(dir), out)
		if err != nil {
			return restored, err
		}
		if ok {
			restored++
		}
	}
	return restored, nil
}

// restoreTrack announces one track if ingest recorded it, and reports
// whether it did.
func restoreTrack(ctx context.Context, objects store.Store, trackPath ledger.TrackPath, out *egress) (bool, error) {
	t, err := ledger.Open(ctx, objects, trackPath, ledger.Config{})
	if err != nil {
		return false, fmt.Errorf("funnel: open %s: %w", trackPath, err)
	}
	schema := t.Root().TrackSchema
	if schema.TimeSource != ledger.TimeSourceIngest || schema.Encoding != ingest.Encoding || schema.MIME != ingest.MIME {
		return false, nil
	}
	dir, name := path.Split(string(trackPath))
	broadcastPath := moqt.BroadcastPath("/" + strings.TrimSuffix(dir, "/"))
	tr := out.announce(broadcastPath).track(moqt.TrackName(name))

	latest, data, err := latestRecord(ctx, t)
	if err != nil {
		return false, fmt.Errorf("funnel: read the latest record of %s: %w", trackPath, err)
	}
	if data != nil {
		tr.publish(group{seq: moqt.GroupSequence(latest.ID.Sequence() + 1), payload: data})
	}
	return true, nil
}

// latestRecord returns the last group committed to t and its content, or nil
// content for a track with no record.
func latestRecord(ctx context.Context, t *ledger.Track) (ledger.GroupInfo, []byte, error) {
	reader, err := t.Reader(ctx)
	if err != nil {
		return ledger.GroupInfo{}, nil, err
	}
	reader.SeekStart()
	var last ledger.GroupInfo
	found := false
	for {
		g, err := reader.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ledger.GroupInfo{}, nil, err
		}
		last, found = g, true
	}
	if !found {
		return ledger.GroupInfo{}, nil, nil
	}
	data, err := reader.ReadGroup(ctx, last.ObjectKey)
	if err != nil {
		return ledger.GroupInfo{}, nil, err
	}
	return last, data, nil
}
