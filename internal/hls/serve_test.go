package hls

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/okdaichi/qumo-ledger/ledger"
	"github.com/okdaichi/qumo-ledger/ledger/store/mem"
	"github.com/qumo-dev/qumo/internal/cmaf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The egress's serving half — a ledger track rendered by streamHandler — works
// without a MoQ source: groups stored as the feed stores them appear in a
// growing playlist, and a segment is the group packaged into a fragment.
func Test_serve_playlist(t *testing.T) {
	ctx := context.Background()
	store := mem.New()
	track, err := ledger.Create(ctx, store, "live/cam1/video", ledger.TrackSchema{
		Timescale:  cmaf.Timescale,
		TimeSource: ledger.TimeSourceIngest,
		Encoding:   encodingMoQLite05,
	}, ledger.Config{})
	require.NoError(t, err)

	writer, err := track.Writer(ctx)
	require.NoError(t, err)

	var mediaTime int64
	for i := range 3 {
		// Three frames 33 ms apart, as storedGroup measures them.
		raw := make([]frame, 3)
		for j := range raw {
			raw[j] = frame{Body: cmaf.EncodeLOC(uint64(j)*33_333, []byte{byte(i), byte(j)})}
		}
		payload, duration, err := storedGroup(raw)
		require.NoError(t, err)
		_, err = writer.AppendGroup(ctx, ledger.GroupInfo{
			ID:        ledger.NewGroupID(0, uint64(i)),
			MediaTime: mediaTime,
			Duration:  duration,
		}, payload)
		require.NoError(t, err)
		mediaTime += duration
	}

	packager, err := cmaf.NewPackager(cmaf.VideoConfig{Codec: "vp09.00.10.08", Width: 1280, Height: 720})
	require.NoError(t, err)
	handler, err := newStreamHandler(track, streamOptions{Packager: packager})
	require.NoError(t, err)
	// The playlist references the groups by their GroupID.
	req := httptest.NewRequest(http.MethodGet, "/playlist.m3u8", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "#EXT-X-PLAYLIST-TYPE:EVENT")
	assert.Contains(t, rec.Body.String(), "e000001-g00000000.m4s")

	// A segment is the group packaged: a fragment covering exactly the
	// duration the playlist lists, 3 frames of 33 ms.
	req = httptest.NewRequest(http.MethodGet, "/e000001-g00000000.m4s", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	frag, err := mp4.DecodeFile(bytes.NewReader(rec.Body.Bytes()))
	require.NoError(t, err)
	require.Len(t, frag.Segments, 1)
	traf := frag.Segments[0].Fragments[0].Moof.Traf
	assert.Equal(t, uint64(3*33_333), traf.Trun.Duration(traf.Tfhd.DefaultSampleDuration),
		"the fragment covers the EXTINF: two gaps and the stated last frame")
}
