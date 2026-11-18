package ingest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/meter"
	"github.com/qumo-dev/qumo/token"
	"github.com/qumo-dev/qumo/usage"
)

// usageSink records the usage records POSTed to it.
type usageSink struct {
	mu      sync.Mutex
	records []usage.Record
}

func (s *usageSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var batch []usage.Record
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, batch...)
}

func (s *usageSink) all() []usage.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]usage.Record(nil), s.records...)
}

// newTestMeter returns a meter sending to a fresh sink.
func newTestMeter(t *testing.T) (*meter.Reporter, *usageSink) {
	t.Helper()
	sink := &usageSink{}
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	m, err := meter.New(meter.Config{Name: "test", URL: srv.URL})
	require.NoError(t, err)
	return m, sink
}

// TestMeterStream verifies a pushed stream is reported under its key: an
// open, and a close carrying the media it took in and its opt-in time.
func TestMeterStream(t *testing.T) {
	m, sink := newTestMeter(t)
	sess, err := NewSession(moqt.NewTrackMux(0), "/live/cam")
	require.NoError(t, err)
	defer sess.Close()

	end := meterStream(m, "k1", sess)
	sess.PushVideo(0, make([]byte, 500), true)
	sess.OptIn(token.OptInABR, time.Time{})
	time.Sleep(1100 * time.Millisecond)
	end(usage.ReasonClosed)
	require.NoError(t, m.Flush(t.Context()))

	records := sink.all()
	require.Len(t, records, 2)
	assert.Equal(t, usage.SessionOpen, records[0].Type)
	closed := records[1]
	assert.Equal(t, usage.SessionClose, closed.Type)
	assert.Equal(t, records[0].SessionID, closed.SessionID)
	assert.Equal(t, "k1", closed.KID)
	assert.Equal(t, usage.ReasonClosed, closed.Reason)
	assert.Equal(t, int64(500), closed.Metrics[usage.IngestMediaBytes])
	assert.Equal(t, int64(1), closed.Metrics[usage.OptInSeconds("abr")])
	assert.NotContains(t, closed.Metrics, usage.OptInSeconds("hls"))
	for _, rec := range records {
		assert.NoError(t, rec.Validate())
	}
}

// TestMeterStream_Unreported verifies a stream with no meter, or no key
// (auth off), is not reported.
func TestMeterStream_Unreported(t *testing.T) {
	m, sink := newTestMeter(t)
	sess, err := NewSession(moqt.NewTrackMux(0), "/live/cam")
	require.NoError(t, err)
	defer sess.Close()

	meterStream(nil, "k1", sess)(usage.ReasonClosed)
	meterStream(m, "", sess)(usage.ReasonClosed)
	require.NoError(t, m.Flush(t.Context()))

	assert.Empty(t, sink.all())
}

// TestRTMPServer_Meter verifies the RTMP server reports a stream under the
// key that granted it, and a refused one not at all.
func TestRTMPServer_Meter(t *testing.T) {
	m, sink := newTestMeter(t)
	authorize := &fakePublishAuthorizer{answer: func(ask publishAsk) (Permission, error) {
		if ask.credential != "tok" {
			return Permission{}, errNoCredential
		}
		return Permission{KeyID: "tenant-key"}, nil
	}}
	mux := moqt.NewTrackMux(0)
	server := NewRTMPServer(RTMPConfig{Addr: "127.0.0.1:0", TrackMux: mux, Authorize: authorize.authorize, Meter: m})
	addr := serveRTMP(t, server)

	publishRTMP(t, addr, "live", "refused?jwt=nope")
	publishRTMP(t, addr, "live", "cam?jwt=tok")
	require.Eventually(t, func() bool { return published(mux, "/live/cam") }, 3*time.Second, 10*time.Millisecond)
	require.NoError(t, m.Flush(t.Context()))

	records := sink.all()
	require.Len(t, records, 1, "an open for the stream granted; none for the refused one")
	assert.Equal(t, usage.SessionOpen, records[0].Type)
	assert.Equal(t, "tenant-key", records[0].KID)
}
