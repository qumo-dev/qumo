package relay

import "github.com/qumo-dev/gomoqt/moqt"

// fakeDropWriter records the SUBSCRIBE_DROP messages a dropReporter sends.
// A dropReporter is used by one egress goroutine only, so the fake is never
// called concurrently and needs no lock.
type fakeDropWriter struct {
	sent []moqt.SubscribeDrop
	err  error
}

var _ dropWriter = (*fakeDropWriter)(nil)

func (f *fakeDropWriter) DropGroups(drop moqt.SubscribeDrop) error {
	f.sent = append(f.sent, drop)
	return f.err
}
