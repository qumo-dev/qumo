package relay

import "github.com/qumo-dev/gomoqt/moqt"

// fakeGroupCanceler records the codes it was reset with. recentStreams calls
// it from one goroutine only, so it needs no lock.
type fakeGroupCanceler struct {
	codes []moqt.GroupErrorCode
}

var _ groupCanceler = (*fakeGroupCanceler)(nil)

func (f *fakeGroupCanceler) CancelWrite(code moqt.GroupErrorCode) {
	f.codes = append(f.codes, code)
}
