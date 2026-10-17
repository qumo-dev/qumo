package ingest

import (
	"slices"
	"sync"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
)

// publishAsk is one question a fakePublishAuthorizer was asked.
type publishAsk struct {
	credential string
	path       moqt.BroadcastPath
}

// fakePublishAuthorizer answers every publish with lapses and err, and records
// what it was asked. The servers ask from their connection goroutines, so the
// record is guarded.
type fakePublishAuthorizer struct {
	lapses time.Time
	err    error

	mu    sync.Mutex
	asked []publishAsk
}

var _ PublishAuthorizer = (*fakePublishAuthorizer)(nil).authorize

func (f *fakePublishAuthorizer) authorize(credential string, path moqt.BroadcastPath) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, publishAsk{credential: credential, path: path})
	return f.lapses, f.err
}

// questions returns what the fake was asked so far.
func (f *fakePublishAuthorizer) questions() []publishAsk {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}
