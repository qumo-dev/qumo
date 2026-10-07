package ingest

import (
	"sync"
	"time"
)

var _ keepaliver = (*fakeKeepaliver)(nil)

// fakeKeepaliver is an RTSP client as the keepalive loop sees it. The zero
// value advertises no session timeout and sends every keepalive successfully.
type fakeKeepaliver struct {
	timeout time.Duration
	// results are the outcomes of successive SendKeepalive calls. The last
	// one repeats; with none, every call succeeds.
	results []error

	mu   sync.Mutex
	sent int
}

func (f *fakeKeepaliver) SessionTimeout() time.Duration { return f.timeout }

func (f *fakeKeepaliver) SendKeepalive() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent++
	if len(f.results) == 0 {
		return nil
	}
	return f.results[min(f.sent, len(f.results))-1]
}

// keepalives reports how many keepalives have been asked for.
func (f *fakeKeepaliver) keepalives() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent
}
