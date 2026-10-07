package funnel

import (
	"context"
	"fmt"
	"net/url"

	"github.com/okdaichi/qumo-ledger/ledger/store"
	_ "github.com/okdaichi/qumo-ledger/ledger/store/bucket" // s3:
	_ "github.com/okdaichi/qumo-ledger/ledger/store/db"     // postgres:, postgresql:
	_ "github.com/okdaichi/qumo-ledger/ledger/store/fs"     // file:
	_ "github.com/okdaichi/qumo-ledger/ledger/store/mem"    // mem:
)

// openStore opens the ledger store a URI names, and returns it with a name for
// the startup log. The scheme selects the backend:
//
//	""                                memory; records are lost when the process exits
//	file:///var/lib/qumo              the directory /var/lib/qumo
//	postgres://user@host:26257/qumo   a table in PostgreSQL or CockroachDB
//	s3://bucket/prefix?region=...     a bucket of S3 or an S3-compatible service
//
// Any other value is an error, including a bare path: the backend is always
// stated.
func openStore(ctx context.Context, uri string) (store.Store, string, error) {
	objects, err := store.Open(ctx, uri)
	if err != nil {
		return nil, "", fmt.Errorf("open LEDGER_URI: %w", err)
	}
	if uri == "" {
		return objects, "memory (lost on exit)", nil
	}
	u, err := url.Parse(uri)
	if err != nil {
		return nil, "", fmt.Errorf("open LEDGER_URI: %w", err)
	}
	return objects, u.Redacted(), nil
}
