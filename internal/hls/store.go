package hls

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

const defaultLedgerURI = "file:ledger"

// openStore opens the ledger store that LEDGER_URI names, and returns it with a
// name for the startup log. The scheme selects the backend:
//
//	s3://bucket/prefix?region=...      Amazon S3, Cloudflare R2, or MinIO
//	file:///path/to/dir                directory on the filesystem
//	file:ledger                        relative directory on the filesystem (default)
//	postgres://user@host:26257/qumo    PostgreSQL or CockroachDB table
//	mem:                               memory (records are lost on exit)
//
// Empty uri defaults to "file:ledger".
func openStore(ctx context.Context, uri string) (store.Store, string, error) {
	if uri == "" {
		uri = defaultLedgerURI
	}
	objects, err := store.Open(ctx, uri)
	if err != nil {
		return nil, "", fmt.Errorf("open LEDGER_URI: %w", err)
	}
	u, err := url.Parse(uri)
	if err != nil {
		return nil, "", fmt.Errorf("open LEDGER_URI: %w", err)
	}
	return objects, u.Redacted(), nil
}
