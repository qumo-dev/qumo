package relay

import (
	"sync"

	"github.com/qumo-dev/gomoqt/moqt"
)

var _ leasedSession = (*fakeLeasedSession)(nil)

// fakeLeasedSession records each CloseWithError and reports the byte totals
// set with setStats. A lease calls it from its timer's goroutine, so it is
// guarded.
type fakeLeasedSession struct {
	mu     sync.Mutex
	closes []sessionClose
	stats  moqt.SessionStats
}

type sessionClose struct {
	code moqt.SessionErrorCode
	msg  string
}

func (f *fakeLeasedSession) CloseWithError(code moqt.SessionErrorCode, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes = append(f.closes, sessionClose{code: code, msg: msg})
	return nil
}

func (f *fakeLeasedSession) Stats() moqt.SessionStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats
}

// setStats sets the byte totals Stats reports from now on.
func (f *fakeLeasedSession) setStats(sent, received uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stats.BytesSent, f.stats.BytesReceived = sent, received
}

// closed returns a copy of the closes so far.
func (f *fakeLeasedSession) closed() []sessionClose {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sessionClose(nil), f.closes...)
}
