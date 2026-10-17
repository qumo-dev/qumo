package funnel

import (
	"context"
	"fmt"

	_ "github.com/okdaichi/qumo-ledger/ledger"
	"github.com/okdaichi/qumo-ledger/ledger/store"
)

// openStore opens the ledger store that uri names, returning the store and its
// description for the startup log. An empty uri opens in-memory storage.
func openStore(ctx context.Context, uri string) (store.Store, string, error) {
	objects, err := store.Open(ctx, uri)
	if err != nil {
		return nil, "", fmt.Errorf("open LEDGER_URI: %w", err)
	}
	return objects, fmt.Sprint(objects), nil
}
