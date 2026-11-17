package meter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/usage"
)

// fakeSink records the batches POSTed to it and answers from a status queue:
// in order, the last repeating; empty means 200.
type fakeSink struct {
	mu       sync.Mutex
	statuses []int
	batches  [][]usage.Record
	gotAuth  string
}

func (f *fakeSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotAuth = r.Header.Get("Authorization")
	var batch []usage.Record
	if err := json.NewDecoder(r.Body).Decode(&batch); err == nil {
		f.batches = append(f.batches, batch)
	}
	status := http.StatusOK
	if len(f.statuses) > 0 {
		status = f.statuses[0]
		if len(f.statuses) > 1 {
			f.statuses = f.statuses[1:]
		}
	}
	w.WriteHeader(status)
}

func (f *fakeSink) received() [][]usage.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]usage.Record(nil), f.batches...)
}

func newTestReporter(t *testing.T, sink *fakeSink) *Reporter {
	t.Helper()
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	r, err := New(Config{Name: "test", URL: srv.URL, Token: "t"})
	require.NoError(t, err)
	return r
}

// summary is each record of batch as "type session_id" or "type kid".
func summary(batch []usage.Record) []string {
	var out []string
	for _, rec := range batch {
		id := rec.SessionID
		if rec.Type == usage.Viewers {
			id = rec.KID
		}
		out = append(out, string(rec.Type)+" "+id)
	}
	return out
}

func bytesMetrics(in, out int64) map[string]int64 {
	return map[string]int64{usage.IngressBytes: in, usage.EgressBytes: out}
}

func TestNew(t *testing.T) {
	for rawURL, wantErr := range map[string]bool{
		"https://usage.example/v1":  false,
		"http://127.0.0.1:9000/v1":  false,
		"http://localhost/v1":       false,
		"http://usage.example/v1":   true,
		"ftp://usage.example/v1":    true,
		"https://usage.example/%zz": true,
	} {
		_, err := New(Config{URL: rawURL})
		assert.Equal(t, wantErr, err != nil, rawURL)
	}
}

// A session's records go out in order: opens, usage, totals, closes, each
// valid, with the bearer token.
func TestReporter_Records(t *testing.T) {
	sink := &fakeSink{}
	r := newTestReporter(t, sink)
	r.Open("s1", "k1")
	r.Open("s2", "k1")
	r.Report("s1", "k1", bytesMetrics(10, 1))
	r.Report("s1", "k1", bytesMetrics(20, 2))
	r.Total("k2", bytesMetrics(0, 7))
	r.Close("s2", "k1", bytesMetrics(5, 5), usage.ReasonClosed)

	require.NoError(t, r.Flush(context.Background()))

	batches := sink.received()
	require.Len(t, batches, 1)
	assert.Equal(t, []string{"session_open s1", "session_open s2", "usage s1", "viewers k2", "session_close s2"}, summary(batches[0]))
	for _, rec := range batches[0] {
		assert.NoError(t, rec.Validate())
	}
	assert.Equal(t, bytesMetrics(20, 2), batches[0][2].Metrics, "usage is coalesced to the latest report")
	assert.Equal(t, r.RunID(), batches[0][3].Run)
	assert.Equal(t, usage.ReasonClosed, batches[0][4].Reason)
	assert.Equal(t, "Bearer t", sink.gotAuth)
}

func TestReporter_CloseSupersedesUsage(t *testing.T) {
	sink := &fakeSink{}
	r := newTestReporter(t, sink)
	r.Open("s1", "k1")
	r.Report("s1", "k1", bytesMetrics(10, 1))
	r.Close("s1", "k1", bytesMetrics(30, 3), usage.ReasonClosed)

	require.NoError(t, r.Flush(context.Background()))

	assert.Equal(t, []string{"session_open s1", "session_close s1"}, summary(sink.received()[0]))
}

// Usage and a close for a session not open, closed already or forgotten are
// ignored.
func TestReporter_NotOpen(t *testing.T) {
	sink := &fakeSink{}
	r := newTestReporter(t, sink)
	r.Report("nope", "k1", bytesMetrics(1, 1))
	r.Close("nope", "k1", bytesMetrics(1, 1), usage.ReasonClosed)
	r.Open("s1", "k1")
	r.Close("s1", "k1", bytesMetrics(1, 1), usage.ReasonClosed)
	r.Close("s1", "k1", bytesMetrics(2, 2), usage.ReasonClosed)
	r.Report("s1", "k1", bytesMetrics(3, 3))
	r.Open("s2", "k1")
	r.Forget("s2")
	r.Report("s2", "k1", bytesMetrics(3, 3))

	require.NoError(t, r.Flush(context.Background()))

	assert.Equal(t, []string{"session_open s1", "session_open s2", "session_close s1"}, summary(sink.received()[0]))
}

func TestReporter_FailedSendIsRetriedWithNewerUsage(t *testing.T) {
	sink := &fakeSink{statuses: []int{http.StatusBadGateway, http.StatusOK}}
	r := newTestReporter(t, sink)
	r.Open("s1", "k1")
	r.Report("s1", "k1", bytesMetrics(10, 0))
	assert.Error(t, r.Flush(context.Background()))
	r.Report("s1", "k1", bytesMetrics(20, 0))

	require.NoError(t, r.Flush(context.Background()))

	retry := sink.received()[1]
	assert.Equal(t, []string{"session_open s1", "usage s1"}, summary(retry), "the open is resent")
	assert.Equal(t, int64(20), retry[1].Metrics[usage.IngressBytes], "the newer report wins")
}

// A forgotten session's usage goes once, but not again after a failure.
func TestReporter_ForgottenUsageIsNotResent(t *testing.T) {
	sink := &fakeSink{statuses: []int{http.StatusOK, http.StatusBadGateway, http.StatusOK}}
	r := newTestReporter(t, sink)
	r.Open("s1", "k1")
	require.NoError(t, r.Flush(context.Background()))
	r.Report("s1", "k1", bytesMetrics(10, 0))
	r.Forget("s1")

	assert.Error(t, r.Flush(context.Background()))
	require.NoError(t, r.Flush(context.Background()))

	assert.Len(t, sink.received(), 2, "the open, then the usage once; nothing to resend")
	assert.Equal(t, []string{"usage s1"}, summary(sink.received()[1]))
}

func TestReporter_MalformedBatchIsDropped(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			sink := &fakeSink{statuses: []int{status, http.StatusOK}}
			r := newTestReporter(t, sink)
			r.Open("s1", "k1")
			require.NoError(t, r.Flush(context.Background()))
			require.NoError(t, r.Flush(context.Background()))
			assert.Len(t, sink.received(), 1)
		})
	}
}

// A rotated token (401, 403), a body over the receiver's limit (413), a
// timeout (408) or throttling (429) lose nothing: the records are kept.
func TestReporter_FailureKeepsTheRecords(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout,
		http.StatusRequestEntityTooLarge, http.StatusTooManyRequests, http.StatusBadGateway} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			sink := &fakeSink{statuses: []int{status, http.StatusOK}}
			r := newTestReporter(t, sink)
			r.Open("s1", "k1")
			assert.Error(t, r.Flush(context.Background()))
			require.NoError(t, r.Flush(context.Background()))
			batches := sink.received()
			require.Len(t, batches, 2)
			assert.Equal(t, batches[0], batches[1], "the same records again")
		})
	}
}

func TestReporter_Batches(t *testing.T) {
	t.Run("a backlog goes out in batches", func(t *testing.T) {
		sink := &fakeSink{}
		r := newTestReporter(t, sink)
		for i := range 1200 {
			r.Open(fmt.Sprintf("s%d", i), "k1")
		}
		require.NoError(t, r.Flush(context.Background()))
		var sizes []int
		for _, b := range sink.received() {
			sizes = append(sizes, len(b))
		}
		assert.Equal(t, []int{500, 500, 200}, sizes)
	})

	t.Run("a failure mid-backlog resends only what wasn't sent", func(t *testing.T) {
		sink := &fakeSink{statuses: []int{http.StatusOK, http.StatusServiceUnavailable, http.StatusOK}}
		r := newTestReporter(t, sink)
		for i := range 700 {
			r.Open(fmt.Sprintf("s%d", i), "k1")
		}
		assert.Error(t, r.Flush(context.Background()))
		require.NoError(t, r.Flush(context.Background()))
		var sizes []int
		for _, b := range sink.received() {
			sizes = append(sizes, len(b))
		}
		assert.Equal(t, []int{500, 200, 200}, sizes, "the first 500 went through; the next 200 are resent once")
	})
}

func TestReporter_Totals(t *testing.T) {
	sink := &fakeSink{statuses: []int{http.StatusOK, http.StatusServiceUnavailable, http.StatusOK}}
	r := newTestReporter(t, sink)
	r.Total("k1", bytesMetrics(0, 100))
	require.NoError(t, r.Flush(context.Background()))

	r.Total("k1", bytesMetrics(0, 100))
	require.NoError(t, r.Flush(context.Background()))
	assert.Len(t, sink.received(), 1, "an unchanged total isn't sent again")

	r.Total("k1", bytesMetrics(0, 150))
	assert.Error(t, r.Flush(context.Background()))
	r.Total("k1", bytesMetrics(0, 160))
	require.NoError(t, r.Flush(context.Background()))

	batches := sink.received()
	require.Len(t, batches, 3)
	require.Len(t, batches[2], 1, "one record, not the old total and the new")
	assert.Equal(t, int64(160), batches[2][0].Metrics[usage.EgressBytes])
}

func TestReporter_EachRunHasItsOwnTotals(t *testing.T) {
	a, b := newTestReporter(t, &fakeSink{}), newTestReporter(t, &fakeSink{})
	assert.NotEqual(t, a.RunID(), b.RunID(), "a restarted server starts new totals rather than lowering old ones")
}

// Run sends every flushInterval, and once more when ctx ends.
func TestReporter_Run(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var sent []time.Time
		r, err := New(Config{Name: "test", URL: "http://127.0.0.1/usage"})
		require.NoError(t, err)
		r.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			sent = append(sent, time.Now())
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		})}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		start := time.Now()
		go func() {
			defer close(done)
			r.Run(ctx)
		}()

		r.Open("s1", "k1")
		time.Sleep(flushInterval + time.Second)
		r.Close("s1", "k1", bytesMetrics(1, 1), usage.ReasonClosed)
		time.Sleep(time.Second)
		cancel()
		<-done

		assert.Equal(t, []time.Time{start.Add(flushInterval), start.Add(flushInterval + 2*time.Second)}, sent,
			"at the tick, then the close at shutdown")
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
