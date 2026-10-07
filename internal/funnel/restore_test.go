package funnel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/okdaichi/qumo-ledger/ingest"
	"github.com/okdaichi/qumo-ledger/ledger"
	"github.com/okdaichi/qumo-ledger/ledger/store"
	"github.com/okdaichi/qumo-ledger/ledger/store/memstore"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordInto announces name into the chat track of broadcastPath through an
// ingest handler over objects, then records each payload.
func recordInto(tb testing.TB, objects store.Store, broadcastPath, name string, payloads ...string) {
	tb.Helper()
	h, err := ingest.NewHandler(objects, ingest.Options{})
	require.NoError(tb, err)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/announce", strings.NewReader(
		`{"broadcast_path":"`+broadcastPath+`","track_name":"chat","name":"`+name+`"}`)))
	require.Equal(tb, http.StatusCreated, rr.Code, rr.Body.String())
	for _, payload := range payloads {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/"+rr.Header().Get("Location")+"/records", strings.NewReader(payload)))
		require.Equal(tb, http.StatusCreated, rec.Code, rec.Body.String())
	}
}

func TestRestore(t *testing.T) {
	objects := memstore.New()
	recordInto(t, objects, "/room/123", "alice", `"one"`, `"two"`)
	recordInto(t, objects, "/room/9", "bob")
	_, err := ledger.Create(t.Context(), objects, "live/cam1/video", ledger.TrackSchema{
		Timescale: 90000, TimeSource: ledger.TimeSourceFrame, MIME: "video/mp4", Encoding: "fmp4",
	}, ledger.Config{})
	require.NoError(t, err)
	mux := moqt.NewTrackMux(0)
	out := newEgress(t.Context(), mux)

	restored, err := restore(t.Context(), objects, out)

	require.NoError(t, err)
	assert.Equal(t, 2, restored, "a track ingest did not record is left alone")

	_, handler := mux.TrackHandler("/room/123")
	require.NotNil(t, handler)
	chat, ok := handler.(*broadcast).lookup("chat")
	require.True(t, ok)
	require.NotNil(t, chat.latest, "the latest record is replayed to a new subscriber")
	assert.Equal(t, moqt.GroupSequence(2), chat.latest.seq)
	assert.JSONEq(t, `{"name":"alice","payload":"two"}`, string(chat.latest.payload))

	_, handler = mux.TrackHandler("/room/9")
	require.NotNil(t, handler)
	empty, ok := handler.(*broadcast).lookup("chat")
	require.True(t, ok, "an announced track with no record is restored")
	assert.Nil(t, empty.latest)

	ann, _ := mux.TrackHandler("/live/cam1")
	assert.Nil(t, ann)
}

func TestRestore_StoreWithoutListing(t *testing.T) {
	objects := memstore.New()
	recordInto(t, objects, "/room/123", "alice", `"one"`)
	mux := moqt.NewTrackMux(0)

	restored, err := restore(t.Context(), fakeUnlistedStore{objects}, newEgress(t.Context(), mux))

	require.NoError(t, err)
	assert.Zero(t, restored)
	ann, _ := mux.TrackHandler("/room/123")
	assert.Nil(t, ann)
}

func TestRestore_ListingFails(t *testing.T) {
	restored, err := restore(t.Context(), fakeFailingLister{}, newEgress(t.Context(), moqt.NewTrackMux(0)))

	assert.Zero(t, restored)
	assert.ErrorIs(t, err, errListing)
}

func TestNewHandler_RestoresBeforeServing(t *testing.T) {
	objects := memstore.New()
	recordInto(t, objects, "/room/123", "alice", `"before the restart"`)
	mux := moqt.NewTrackMux(0)

	_, err := NewHandler(t.Context(), objects, mux, nil)

	require.NoError(t, err)
	_, handler := mux.TrackHandler("/room/123")
	require.NotNil(t, handler)
	chat, ok := handler.(*broadcast).lookup("chat")
	require.True(t, ok)
	require.NotNil(t, chat.latest)
	assert.JSONEq(t, `{"name":"alice","payload":"before the restart"}`, string(chat.latest.payload))

	_, err = NewHandler(t.Context(), fakeFailingLister{}, moqt.NewTrackMux(0), nil)
	assert.ErrorIs(t, err, errListing)
}
