package relay

import (
	"sync"

	"github.com/qumo-dev/gomoqt/moqt"
)

var _ sessionCloser = (*fakeSessionCloser)(nil)

// fakeSessionCloser records each CloseWithError. It is called from a timer's
// goroutine, so the record is guarded.
type fakeSessionCloser struct {
	mu     sync.Mutex
	closes []sessionClose
}

type sessionClose struct {
	code moqt.SessionErrorCode
	msg  string
}

func (f *fakeSessionCloser) CloseWithError(code moqt.SessionErrorCode, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes = append(f.closes, sessionClose{code: code, msg: msg})
	return nil
}

// closed returns a copy of the closes so far.
func (f *fakeSessionCloser) closed() []sessionClose {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sessionClose(nil), f.closes...)
}
