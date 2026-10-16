package funnel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/okdaichi/qumo-ledger/ingest"
	"github.com/okdaichi/qumo-ledger/ledger/store/mem"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/token"
)

// chatURL is the chat track of /room/123, as the handler addresses it.
const chatURL = "/tracks/room/123/chat"

// serve sends one request to h, with credential as a bearer token unless it is
// empty, and returns the response.
func serve(h http.Handler, method, target, body string, credential ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if len(credential) > 0 {
		req.Header.Set("Authorization", "Bearer "+credential[0])
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestNewHandler_PublishesCommittedRecords(t *testing.T) {
	mux := moqt.NewTrackMux(0)
	h, err := NewHandler(t.Context(), mem.New(), mux, HandlerOptions{})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, serve(h, http.MethodPut, chatURL, "").Code)

	for _, payload := range []string{`{"text":"hello"}`, `"again"`} {
		rr := serve(h, http.MethodPost, chatURL, payload)
		require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	}

	_, handler := mux.TrackHandler("/room/123")
	require.NotNil(t, handler)
	chat, ok := handler.(*broadcast).lookup("chat")
	require.True(t, ok)
	require.NotNil(t, chat.latest)
	assert.Equal(t, moqt.GroupSequence(2), chat.latest.seq, "the second ledger group (sequence 1) is group 2")
	assert.Equal(t, `{"payload":"again"}`, string(chat.latest.payload), "the record is sent as it was stored")
}

// postAs grants subject posting into the tracks of the broadcast at path.
func postAs(subject, path string) token.Grant {
	g := exactly(path, token.ActionPost)
	g.Subject = subject
	return g
}

// createOn grants creating tracks of the broadcast at path.
func createOn(path string) token.Grant {
	return exactly(path, token.ActionCreate)
}

// exactly grants actions on the broadcast at path, "a/b", alone: the pattern
// "/b" beneath the root "a".
func exactly(path string, actions ...token.Action) token.Grant {
	root, base := "", path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		root, base = path[:i], path[i+1:]
	}
	return grantOn(root, []string{"/" + base}, actions...)
}

// grantOn grants actions on patterns beneath root.
func grantOn(root string, patterns []string, actions ...token.Action) token.Grant {
	g := token.Grant{Root: root}
	for _, a := range actions {
		switch a {
		case token.ActionPublish:
			g.Publish = patterns
		case token.ActionSubscribe:
			g.Subscribe = patterns
		case token.ActionCreate:
			g.Create = patterns
		case token.ActionPost:
			g.Post = patterns
		case token.ActionRedact:
			g.Redact = patterns
		}
	}
	return g
}

func TestNewHandler_SenderComesFromTheCredential(t *testing.T) {
	v, key := newVerifier(t)
	mux := moqt.NewTrackMux(0)
	h, err := NewHandler(t.Context(), mem.New(), mux, HandlerOptions{Verifier: v})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, serve(h, http.MethodPut, chatURL, "", sign(t, key, createOn("room/123"))).Code)

	rr := serve(h, http.MethodPost, chatURL, `{"user":"mallory","text":"hi"}`,
		sign(t, key, postAs("user-42", "room/123")))

	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	_, handler := mux.TrackHandler("/room/123")
	require.NotNil(t, handler)
	chat, ok := handler.(*broadcast).lookup("chat")
	require.True(t, ok)
	assert.JSONEq(t, `{"sender":"user-42","payload":{"user":"mallory","text":"hi"}}`, string(chat.latest.payload))
}

func TestNewHandler_CreatePublishesTheTrack(t *testing.T) {
	mux := moqt.NewTrackMux(0)
	h, err := NewHandler(t.Context(), mem.New(), mux, HandlerOptions{})
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

func TestNewHandler_PostingNeedsACreatedTrack(t *testing.T) {
	v, key := newVerifier(t)
	mux := moqt.NewTrackMux(0)
	h, err := NewHandler(t.Context(), mem.New(), mux, HandlerOptions{Verifier: v})
	require.NoError(t, err)
	poster := sign(t, key, postAs("user-42", "room/123"))

	posted := serve(h, http.MethodPost, chatURL, `"hi"`, poster)
	created := serve(h, http.MethodPut, chatURL, "", poster)

	assert.Equal(t, http.StatusNotFound, posted.Code, "posting never creates a track")
	assert.Equal(t, http.StatusForbidden, created.Code, "posting does not grant creating")
	ann, _ := mux.TrackHandler("/room/123")
	assert.Nil(t, ann, "nothing is published")
}

func TestNewHandler_RefusedRecordIsNotPublished(t *testing.T) {
	v, _ := newVerifier(t)
	mux := moqt.NewTrackMux(0)
	h, err := NewHandler(t.Context(), mem.New(), mux, HandlerOptions{Verifier: v})
	require.NoError(t, err)

	rr := serve(h, http.MethodPost, chatURL, `"unsigned"`)

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
	assert.Equal(t, "Bearer", rr.Header().Get("WWW-Authenticate"))
	ann, _ := mux.TrackHandler("/room/123")
	assert.Nil(t, ann)
}

func TestNewHandler_HistoryNeedsASubscriber(t *testing.T) {
	v, key := newVerifier(t)
	h, err := NewHandler(t.Context(), mem.New(), moqt.NewTrackMux(0), HandlerOptions{Verifier: v})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, serve(h, http.MethodPut, chatURL, "", sign(t, key, createOn("room/123"))).Code)
	require.Equal(t, http.StatusCreated,
		serve(h, http.MethodPost, chatURL, `"hello"`, sign(t, key, postAs("user-42", "room/123"))).Code)

	viewer := serve(h, http.MethodGet, chatURL, "", sign(t, key, grantOn("room", []string{"/123", "/123/"}, token.ActionSubscribe)))
	sender := serve(h, http.MethodGet, chatURL, "", sign(t, key, postAs("user-42", "room/123")))

	require.Equal(t, http.StatusOK, viewer.Code)
	assert.Contains(t, viewer.Body.String(), `"sender":"user-42","payload":"hello"`)
	assert.Equal(t, http.StatusForbidden, sender.Code, "posting does not grant reading")
}

func TestNewHandler_Limits(t *testing.T) {
	h, err := NewHandler(t.Context(), mem.New(), moqt.NewTrackMux(0), HandlerOptions{
		TrackLimit: ingest.Limit{Rate: 1, Burst: 1},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, serve(h, http.MethodPut, chatURL, "").Code)

	assert.Equal(t, http.StatusCreated, serve(h, http.MethodPost, chatURL, `"one"`).Code)
	assert.Equal(t, http.StatusTooManyRequests, serve(h, http.MethodPost, chatURL, `"two"`).Code)
}

func TestParseLimit(t *testing.T) {
	tests := map[string]struct {
		in      string
		want    ingest.Limit
		wantErr bool
	}{
		"none":            {in: ""},
		"rate and burst":  {in: "1,5", want: ingest.Limit{Rate: 1, Burst: 5}},
		"a fraction":      {in: " 0.5 , 2 ", want: ingest.Limit{Rate: 0.5, Burst: 2}},
		"no burst":        {in: "1", wantErr: true},
		"zero rate":       {in: "0,5", wantErr: true},
		"zero burst":      {in: "1,0", wantErr: true},
		"not numbers":     {in: "fast,big", wantErr: true},
		"a negative rate": {in: "-1,5", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := parseLimit(tt.in)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
