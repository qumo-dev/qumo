package funnel

import (
	"net/url"
	"path/filepath"
	"testing"

	"github.com/okdaichi/qumo-ledger/ledger/store"
	"github.com/okdaichi/qumo-ledger/ledger/store/fs"
	"github.com/okdaichi/qumo-ledger/ledger/store/mem"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenStore_EmptyIsMemory(t *testing.T) {
	objects, name, err := openStore(t.Context(), "")

	require.NoError(t, err)
	assert.IsType(t, &mem.Store{}, objects)
	assert.Contains(t, name, "memory")
}

func TestOpenStore_FileIsADirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	uri := (&url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(dir)}).String()

	objects, name, err := openStore(t.Context(), uri)

	require.NoError(t, err)
	assert.IsType(t, &fs.Store{}, objects)
	assert.Equal(t, uri, name)
	assert.DirExists(t, dir)
}

func TestOpenStore_RegistersEveryBackend(t *testing.T) {
	assert.Subset(t, store.Schemes(), []string{"mem", "file", "postgres", "postgresql", "s3"})
}

func TestOpenStore_Rejected(t *testing.T) {
	tests := map[string]string{
		"a bare path":        "./ledger",
		"an unknown scheme":  "gopher://host/ledger",
		"a file URI on host": "file://example.com/var/lib/qumo",
		"a file URI no path": "file://",
	}
	for name, uri := range tests {
		t.Run(name, func(t *testing.T) {
			objects, _, err := openStore(t.Context(), uri)

			assert.Error(t, err)
			assert.Nil(t, objects)
		})
	}
}
