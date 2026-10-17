package hls

import (
	"slices"
	"sync"

	"github.com/qumo-dev/gomoqt/moqt"
)

// viewerAsk is one question a fakeViewerCheck was asked.
type viewerAsk struct {
	credential string
	path       moqt.BroadcastPath
}

// fakeViewerCheck answers every viewer with err, and records what it was
// asked. The HTTP server asks from its request goroutines, so the record is
// guarded.
type fakeViewerCheck struct {
	err error

	mu    sync.Mutex
	asked []viewerAsk
}

func (f *fakeViewerCheck) check(credential string, path moqt.BroadcastPath) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, viewerAsk{credential: credential, path: path})
	return f.err
}

// questions returns what the fake was asked so far.
func (f *fakeViewerCheck) questions() []viewerAsk {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}
