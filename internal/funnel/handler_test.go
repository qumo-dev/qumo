package funnel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/okdaichi/qumo-ledger/ledger/store/memstore"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chatURL is the chat track of /room/123, as the handler addresses it.
const chatURL = "/tracks/room/123/chat"

// serve sends one request to h and returns the response.
func serve(h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(method, target, strings.NewReader(body)))
	return rr
}

func TestNewHandler_PublishesCommittedRecords(t *testing.T) {
	mux := moqt.NewTrackMux(0)
	h, err := NewHandler(t.Context(), memstore.New(), mux, nil)
	require.NoError(t, err)

	for _, payload := range []string{`{"user":"alice","text":"hello"}`, `"again"`} {
		rr := serve(h, http.MethodPost, chatURL, payload)
		require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	}

	_, handler := mux.TrackHandler("/room/123")
	require.NotNil(t, handler, "the first record publishes the broadcast")
	chat, ok := handler.(*broadcast).lookup("chat")
	require.True(t, ok)
	require.NotNil(t, chat.latest)
	assert.Equal(t, moqt.GroupSequence(2), chat.latest.seq, "the second ledger group (sequence 1) is group 2")
	assert.Equal(t, `"again"`, string(chat.latest.payload), "the payload is sent as it was recorded")
}

func TestNewHandler_CreatePublishesTheTrack(t *testing.T) {
	mux := moqt.NewTrackMux(0)
	h, err := NewHandler(t.Context(), memstore.New(), mux, nil)
	require.NoError(t, err)

	rr := serve(h, http.MethodPut, chatURL, "")

	require.Equal(t, http.StatusCreated, rr.Code)
	_, handler := mux.TrackHandler("/room/123")
	require.NotNil(t, handler)
	chat, ok := handler.(*broadcast).lookup("chat")
	require.True(t, ok, "a subscriber to the created track waits for its first record")
	assert.Nil(t, chat.latest)
	_, ok = handler.(*broadcast).lookup("nosuch")
	assert.False(t, ok, "a subscriber to any other name is not found")
}

func TestNewHandler_RefusedRecordIsNotPublished(t *testing.T) {
	v, _ := newVerifier(t)
	mux := moqt.NewTrackMux(0)
	h, err := NewHandler(t.Context(), memstore.New(), mux, v)
	require.NoError(t, err)

	rr := serve(h, http.MethodPost, chatURL, `"unsigned"`)

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
	assert.Equal(t, "Bearer", rr.Header().Get("WWW-Authenticate"))
	ann, _ := mux.TrackHandler("/room/123")
	assert.Nil(t, ann)
}
