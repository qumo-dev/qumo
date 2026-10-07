package funnel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/okdaichi/qumo-ledger/ledger/store/memstore"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewHandler_PublishesCommittedRecords(t *testing.T) {
	mux := moqt.NewTrackMux(0)
	h, err := NewHandler(t.Context(), memstore.New(), mux, nil)
	require.NoError(t, err)

	announce := httptest.NewRecorder()
	h.ServeHTTP(announce, httptest.NewRequest(http.MethodPost, "/announce",
		strings.NewReader(`{"broadcast_path":"/room/123","track_name":"chat","name":"alice"}`)))
	require.Equal(t, http.StatusCreated, announce.Code)

	_, handler := mux.TrackHandler("/room/123")
	require.NotNil(t, handler, "announcing publishes the broadcast")
	chat := handler.(*broadcast).track("chat")
	assert.Nil(t, chat.latest, "nothing is sent before a record")

	for _, payload := range []string{`{"text":"hello"}`, `"again"`} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/"+announce.Header().Get("Location")+"/records",
			strings.NewReader(payload)))
		require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	}

	require.NotNil(t, chat.latest)
	assert.Equal(t, moqt.GroupSequence(2), chat.latest.seq, "the second ledger group (sequence 1) is group 2")
	var sent struct {
		Name    string          `json:"name"`
		Payload json.RawMessage `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(chat.latest.payload, &sent))
	assert.Equal(t, "alice", sent.Name)
	assert.JSONEq(t, `"again"`, string(sent.Payload))
}

func TestNewHandler_RefusedAnnounceIsNotPublished(t *testing.T) {
	v, _ := newVerifier(t)
	mux := moqt.NewTrackMux(0)
	h, err := NewHandler(t.Context(), memstore.New(), mux, v)
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/announce",
		strings.NewReader(`{"broadcast_path":"/room/123","track_name":"chat","name":"alice"}`)))

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
	assert.Equal(t, "Bearer", rr.Header().Get("WWW-Authenticate"))
	ann, _ := mux.TrackHandler("/room/123")
	assert.Nil(t, ann)
}
