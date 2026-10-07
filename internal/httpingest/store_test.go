package httpingest

import (
	"net/url"
	"path/filepath"
	"testing"

	"github.com/okdaichi/qumo-ledger/ledger/store/fsstore"
	"github.com/okdaichi/qumo-ledger/ledger/store/memstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenStore_EmptyIsMemory(t *testing.T) {
	objects, name, err := openStore("")

	require.NoError(t, err)
	assert.IsType(t, &memstore.Store{}, objects)
	assert.Contains(t, name, "memory")
}

func TestOpenStore_FileIsADirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	uri := (&url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(dir)}).String()

	objects, name, err := openStore(uri)

	require.NoError(t, err)
	assert.IsType(t, &fsstore.Store{}, objects)
	assert.Equal(t, dir, name)
	assert.DirExists(t, dir)
}

func TestOpenStore_Rejected(t *testing.T) {
	tests := map[string]string{
		"a bare path":        "./ledger",
		"an unknown scheme":  "s3://bucket/ledger",
		"a file URI on host": "file://example.com/var/lib/qumo",
		"a file URI no path": "file://",
	}
	for name, uri := range tests {
		t.Run(name, func(t *testing.T) {
			objects, _, err := openStore(uri)

			assert.Error(t, err)
			assert.Nil(t, objects)
		})
	}
}

func TestFileURIPath(t *testing.T) {
	tests := map[string]struct {
		uri  string
		want string
	}{
		"absolute":      {uri: "file:///var/lib/qumo", want: filepath.FromSlash("/var/lib/qumo")},
		"relative":      {uri: "file:ledger/data", want: filepath.FromSlash("ledger/data")},
		"windows drive": {uri: "file:///C:/qumo/ledger", want: filepath.FromSlash("C:/qumo/ledger")},
		"localhost":     {uri: "file://localhost/var/lib/qumo", want: filepath.FromSlash("/var/lib/qumo")},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			u, err := url.Parse(tt.uri)
			require.NoError(t, err)

			got, err := fileURIPath(u)

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
