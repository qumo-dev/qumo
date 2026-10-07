package funnel

import (
	"context"
	"errors"
	"iter"

	"github.com/okdaichi/qumo-ledger/ledger/store"
)

// fakeUnlistedStore is the store it wraps without its listing.
type fakeUnlistedStore struct{ store.Store }

var _ store.Store = fakeUnlistedStore{}

// errListing is what fakeFailingLister's listing fails with.
var errListing = errors.New("listing is down")

// fakeFailingLister is a store whose listing fails. It holds no objects.
type fakeFailingLister struct{ fakeUnlistedStore }

var _ store.Lister = fakeFailingLister{}

func (fakeFailingLister) List(context.Context, string) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) { yield("", errListing) }
}
