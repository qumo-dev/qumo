package hls

import (
	"context"
	"testing"

	"github.com/okdaichi/qumo-ledger/ledger"
	"github.com/okdaichi/qumo-ledger/ledger/store/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openTrack creates the track with the ingest schema on first contact and
// adopts an existing one on the second, so the egress can restart against a
// track it (or the seeder) already populated.
func Test_openTrack_createsThenOpens(t *testing.T) {
	store, err := fs.New(t.TempDir())
	require.NoError(t, err)

	const path ledger.TrackPath = "test/video"
	ctx := context.Background()
	schema := ledger.TrackSchema{
		Timescale: 90000, TimeSource: ledger.TimeSourceIngest,
		MIME: "video/mp4", Encoding: "fmp4",
	}

	track, adopted, err := openTrack(ctx, store, path, schema)
	require.NoError(t, err)
	assert.False(t, adopted, "the first call creates the track")
	info := track.Root()
	assert.Equal(t, path, info.Track)
	assert.Equal(t, uint32(90000), info.Timescale)
	assert.Equal(t, ledger.TimeSourceIngest, info.TimeSource)

	again, adopted, err := openTrack(ctx, store, path, schema)
	require.NoError(t, err, "a second openTrack must adopt the existing track")
	assert.True(t, adopted,
		"the caller needs to know it inherited someone else's groups, and must open its own epoch")
	assert.Equal(t, path, again.Root().Track)
}

func Test_envUint(t *testing.T) {
	tests := map[string]struct {
		set  string
		def  uint64
		want uint64
		err  bool
	}{
		"unset uses default": {set: "", def: 7, want: 7},
		"valid value":        {set: "90000", def: 7, want: 90000},
		"invalid value":      {set: "nope", def: 7, err: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HLS_TEST_UINT", tt.set)
			got, err := envUint("HLS_TEST_UINT", tt.def)
			if tt.err {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
