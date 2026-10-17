package hls

import (
	"context"
	"fmt"

	_ "github.com/okdaichi/qumo-ledger/ledger"
	"github.com/okdaichi/qumo-ledger/ledger/store"
)

const defaultLedgerURI = "file:ledger"

// openStore opens the ledger store that uri names, returning the store and its
// description for the startup log. An empty uri defaults to "file:ledger".
func openStore(ctx context.Context, uri string) (store.Store, string, error) {
	if uri == "" {
		uri = defaultLedgerURI
	}
	objects, err := store.Open(ctx, uri)
	if err != nil {
		return nil, "", fmt.Errorf("open LEDGER_URI: %w", err)
	}
	return objects, fmt.Sprint(objects), nil
}
