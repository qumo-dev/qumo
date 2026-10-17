package stream

import (
	"context"
	"fmt"
	"sync"

	"github.com/okdaichi/qumo-ledger/ledger/store"
	"github.com/okdaichi/qumo-ledger/ledger/store/mem"
)

// fakeStore is a memory-backed object store that stands in for an outage:
// failAll fails every Get, failKey fails one key, and err overrides the error
// returned. The default error quotes the key, so a leak into a response body is
// findable. Set the fields before requests are in flight.
//
// The zero value passes everything through.
type fakeStore struct {
	failAll bool
	failKey string
	err     error

	once    sync.Once
	backend *mem.Store
}

var _ store.Store = (*fakeStore)(nil)

func (s *fakeStore) inner() *mem.Store {
	s.once.Do(func() { s.backend = mem.New() })
	return s.backend
}

func (s *fakeStore) Get(ctx context.Context, key string) ([]byte, store.Version, error) {
	if s.failAll || (s.failKey != "" && key == s.failKey) {
		if s.err != nil {
			return nil, store.NoVersion, s.err
		}
		return nil, store.NoVersion, fmt.Errorf("store outage on %q", key)
	}
	return s.inner().Get(ctx, key)
}

func (s *fakeStore) Create(ctx context.Context, key string, data []byte) (store.Version, error) {
	return s.inner().Create(ctx, key, data)
}

func (s *fakeStore) Swap(ctx context.Context, key string, data []byte, expect store.Version) (store.Version, error) {
	return s.inner().Swap(ctx, key, data, expect)
}

func (s *fakeStore) Delete(ctx context.Context, key string) error {
	return s.inner().Delete(ctx, key)
}
