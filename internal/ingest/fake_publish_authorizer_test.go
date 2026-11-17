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

// fakePublishAuthorizer answers every publish with lapses and err, or with
// answer when it is set, and records what it was asked. The servers ask from
// their connection goroutines, so the record is guarded.
type fakePublishAuthorizer struct {
	lapses time.Time
	err    error
	answer func(publishAsk) (time.Time, error)

	mu    sync.Mutex
	asked []publishAsk
}

var _ PublishAuthorizer = (*fakePublishAuthorizer)(nil).authorize

func (f *fakePublishAuthorizer) authorize(credential string, path moqt.BroadcastPath) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ask := publishAsk{credential: credential, path: path}
	f.asked = append(f.asked, ask)
	if f.answer != nil {
		return f.answer(ask)
	}
	return f.lapses, f.err
}

// questions returns what the fake was asked so far.
func (f *fakePublishAuthorizer) questions() []publishAsk {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}
