package hls

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/okdaichi/qumo-ledger/ledger"
	"github.com/okdaichi/qumo-ledger/ledger/store/mem"
	"github.com/okdaichi/qumo-ledger/stream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The egress's serving half — a ledger track rendered by stream.Handler — works
// without a MoQ source: groups written through the ledger appear in a growing
// playlist, and a segment resolves to its appended bytes. The feed (MoQ), the
// packaging into fMP4, and sample-accurate timestamps are separate concerns.
func Test_serve_playlist(t *testing.T) {
	ctx := context.Background()
	store := mem.New()
	track, err := ledger.Create(ctx, store, "live/cam1/video", ledger.TrackSchema{
		Timescale: 90000, TimeSource: ledger.TimeSourceIngest,
		MIME: "video/mp4", Encoding: "fmp4",
	}, ledger.Config{})
	require.NoError(t, err)

	writer, err := track.Writer(ctx)
	require.NoError(t, err)

	const du int64 = 180000 // two seconds at 90 kHz
	payloads := []string{"segment-0", "segment-1", "segment-2"}
	for i, p := range payloads {
		_, err := writer.AppendGroup(ctx, ledger.GroupInfo{
			ID:        ledger.NewGroupID(0, uint64(i)),
			MediaTime: int64(i) * du,
			Duration:  du,
		}, []byte(p))
		require.NoError(t, err)
	}

	// An fmp4 track needs an init segment: without one the manifests would omit
	// #EXT-X-MAP and no player could use them, so the handler refuses to build.
	handler, err := stream.NewHandler(track, stream.Options{
		InitSegment: stream.InitSegment{Bytes: []byte("init-bytes")},
	})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// The playlist references the groups by their GroupID.
	resp, err := http.Get(ts.URL + "/playlist.m3u8")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "#EXT-X-PLAYLIST-TYPE:EVENT")
	assert.Contains(t, string(body), "e000001-g00000000.m4s")

	// A segment request resolves to its appended bytes (proxy delivery).
	resp, err = http.Get(ts.URL + "/e000001-g00000000.m4s")
	require.NoError(t, err)
	seg, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, []byte("segment-0"), seg)
}
