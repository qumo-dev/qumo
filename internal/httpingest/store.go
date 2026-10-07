package httpingest

import (
	"fmt"
	"net/url"
	"path/filepath"

	"github.com/okdaichi/qumo-ledger/ledger/store"
	"github.com/okdaichi/qumo-ledger/ledger/store/fsstore"
	"github.com/okdaichi/qumo-ledger/ledger/store/memstore"
)

// openStore opens the ledger store a URI names, and returns it with a name for
// the startup log. The scheme selects the backend:
//
//	""                      memory; records are lost when the process exits
//	file:///var/lib/qumo    the directory /var/lib/qumo
//	file:ledger             the directory ledger, relative to the working directory
//
// Any other value is an error, including a bare path: the backend is always
// stated.
func openStore(uri string) (store.Store, string, error) {
	if uri == "" {
		return memstore.New(), "memory (lost on exit)", nil
	}
	u, err := url.Parse(uri)
	if err != nil {
		return nil, "", fmt.Errorf("invalid LEDGER_URI %q: %w", uri, err)
	}
	switch u.Scheme {
	case "file":
		dir, err := fileURIPath(u)
		if err != nil {
			return nil, "", fmt.Errorf("invalid LEDGER_URI %q: %w", uri, err)
		}
		objects, err := fsstore.New(dir)
		if err != nil {
			return nil, "", fmt.Errorf("open ledger store %s: %w", dir, err)
		}
		return objects, dir, nil
	default:
		return nil, "", fmt.Errorf("invalid LEDGER_URI %q: unsupported scheme %q (use file://, or leave it empty for memory)", uri, u.Scheme)
	}
}

// fileURIPath returns the directory a file URI names.
func fileURIPath(u *url.URL) (string, error) {
	if u.Host != "" && u.Host != "localhost" {
		return "", fmt.Errorf("a file URI names no host, got %q", u.Host)
	}
	// file:relative/dir carries its path as Opaque; file:///abs/dir as Path.
	dir := u.Opaque
	if dir == "" {
		dir = u.Path
	}
	if dir == "" {
		return "", fmt.Errorf("a file URI needs a path")
	}
	// file:///C:/dir parses to the path /C:/dir.
	if len(dir) >= 3 && dir[0] == '/' && dir[2] == ':' {
		dir = dir[1:]
	}
	return filepath.FromSlash(dir), nil
}
