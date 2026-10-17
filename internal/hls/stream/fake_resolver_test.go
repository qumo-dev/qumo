package stream

import (
	"context"

	"github.com/okdaichi/qumo-ledger/ledger"
)

// fakeResolver is a SegmentResolver that fails every group with err. The zero
// value proxies, like ProxyResolver.
type fakeResolver struct {
	err error
}

var _ SegmentResolver = (*fakeResolver)(nil)

func (f *fakeResolver) ResolveSegment(context.Context, ledger.GroupInfo) (string, error) {
	return "", f.err
}
