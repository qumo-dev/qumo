package relay

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The Config struct itself holds plain data; the behavior that matters is how
// its fields are *consumed*. These tests cover the wiring of GroupCacheSize and
// FrameCapacity into the per-track ring and the per-node frame pool — the parts
// that were previously dead (stored on Config, never read).

// TestNewTrackManager_DefaultResolution verifies that newTrackManager — the
// construction point fed by Config.GroupCacheSize / the Server's frame pool —
// falls back to the package defaults when given zero/negative/nil, and honors
// explicit values otherwise.
func TestNewTrackManager_DefaultResolution(t *testing.T) {
	custom := NewFramePool(2048)
	cases := map[string]struct {
		cacheSize int
		pool      *FramePool
		wantSize  int
		wantPool  *FramePool
	}{
		"unset → defaults":        {cacheSize: 0, pool: nil, wantSize: DefaultGroupCacheSize, wantPool: DefaultFramePool},
		"negative → default size": {cacheSize: -1, pool: nil, wantSize: DefaultGroupCacheSize, wantPool: DefaultFramePool},
		"explicit size":           {cacheSize: 42, pool: nil, wantSize: 42, wantPool: DefaultFramePool},
		"explicit pool":           {cacheSize: 0, pool: custom, wantSize: DefaultGroupCacheSize, wantPool: custom},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tm := newTrackManager(tc.cacheSize, tc.pool)
			assert.Equal(t, tc.wantSize, tm.cacheSize, "cacheSize not resolved")
			assert.Same(t, tc.wantPool, tm.pool, "pool not resolved")
		})
	}
}

// TestResolveFramePool verifies the per-node pool selection: an unset
// (≤0 / nil) Config reuses the shared DefaultFramePool; a positive
// FrameCapacity mints a dedicated, right-sized pool.
func TestResolveFramePool(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		assert.Same(t, DefaultFramePool, resolveFramePool(nil))
	})
	t.Run("unset capacity", func(t *testing.T) {
		assert.Same(t, DefaultFramePool, resolveFramePool(&Config{}))
	})
	t.Run("zero/negative capacity", func(t *testing.T) {
		assert.Same(t, DefaultFramePool, resolveFramePool(&Config{FrameCapacity: -100}))
	})
	t.Run("explicit capacity → dedicated pool", func(t *testing.T) {
		p := resolveFramePool(&Config{FrameCapacity: 4096})
		assert.NotNil(t, p)
		assert.NotSame(t, DefaultFramePool, p, "explicit capacity should mint a dedicated pool")
	})
}

func Test_splitAddrList(t *testing.T) {
	tests := map[string]struct {
		raw  string
		want []string
	}{
		"empty":                {raw: "", want: nil},
		"one":                  {raw: "hub:4433", want: []string{"hub:4433"}},
		"list with whitespace": {raw: " hub1:4433 , hub2:4433 ,", want: []string{"hub1:4433", "hub2:4433"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := splitAddrList(tt.raw)

			assert.Equal(t, tt.want, got)
		})
	}
}
