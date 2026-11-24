package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/internal/meter"
	"github.com/qumo-dev/qumo/token"
	"github.com/qumo-dev/qumo/usage"
)

// fakeUsageSink records the batches POSTed to it and answers from a status
// queue: in order, the last repeating; empty means 200.
type fakeUsageSink struct {
	mu       sync.Mutex
	statuses []int
	batches  [][]usage.Record
	gotAuth  string
}

func (f *fakeUsageSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

func (f *fakeUsageSink) received() [][]usage.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]usage.Record(nil), f.batches...)
}

func newTestMeter(t *testing.T, sink http.Handler) *meter.Reporter {
	t.Helper()
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	m, err := meter.New(meter.Config{Name: "relay", URL: srv.URL, Token: "t"})
	require.NoError(t, err)
	return m
}

func newTestUsage(t *testing.T, sink http.Handler) *usageReporter {
	t.Helper()
	return newUsageReporter(newTestMeter(t, sink))
}

var testUsageSession = usageSession{kid: "k1", publishes: true}

func viewerSession(kid string) usageSession {
	return usageSession{kid: kid}
}

func TestUsageReporter(t *testing.T) {
	t.Run("usage is coalesced to the latest report", func(t *testing.T) {
		sink := &fakeUsageSink{}
		r := newTestUsage(t, sink)
		r.open("s1", testUsageSession)
		r.revalidated("s1", auth.Bytes{Sent: 1, Received: 10}, nil, true)
		r.revalidated("s1", auth.Bytes{Sent: 2, Received: 20}, nil, true)
		require.NoError(t, r.meter.Flush(context.Background()))
		batches := sink.received()
		require.Len(t, batches, 1)
		require.Len(t, batches[0], 2)
		assert.Equal(t, usage.SessionOpen, batches[0][0].Type)
		assert.Equal(t, int64(20), batches[0][1].Metrics["gateway.ingress_bytes"])
	})

	t.Run("a failed send is retried with newer usage", func(t *testing.T) {
		sink := &fakeUsageSink{statuses: []int{http.StatusBadGateway, http.StatusOK}}
		r := newTestUsage(t, sink)
		r.open("s1", testUsageSession)
		r.revalidated("s1", auth.Bytes{Received: 10}, nil, true)
		assert.Error(t, r.meter.Flush(context.Background()))
		r.revalidated("s1", auth.Bytes{Received: 20}, nil, true)
		require.NoError(t, r.meter.Flush(context.Background()))
		retry := sink.received()[1]
		require.Len(t, retry, 2)
		assert.Equal(t, usage.SessionOpen, retry[0].Type, "the open is resent")
		assert.Equal(t, int64(20), retry[1].Metrics["gateway.ingress_bytes"], "the newer report wins")
	})

	// Batching, retries and what each answer from the receiver does are
	// package meter's, tested there.

	t.Run("an unknown session's end and usage are ignored", func(t *testing.T) {
		sink := &fakeUsageSink{}
		r := newTestUsage(t, sink)
		assert.False(t, r.close("nope", auth.Bytes{Sent: 1}, nil, ""))
		r.revalidated("nope", auth.Bytes{Sent: 1}, nil, true)
		require.NoError(t, r.meter.Flush(context.Background()))
		assert.Empty(t, sink.received(), "nothing pending, nothing sent")
	})

	t.Run("a session whose end never came is forgotten once no longer revalidated", func(t *testing.T) {
		now := time.Now()
		r := newTestUsage(t, &fakeUsageSink{})
		r.now = func() time.Time { return now }
		r.open("gone", testUsageSession)
		r.open("refused", testUsageSession)
		r.open("live", testUsageSession)

		now = now.Add(sessionGrace + time.Second)
		r.revalidated("refused", auth.Bytes{Sent: 1}, nil, false)
		r.revalidated("live", auth.Bytes{}, nil, true)
		r.forgetStale()

		assert.False(t, r.close("gone", auth.Bytes{}, nil, ""), "not revalidated within the grace")
		assert.False(t, r.close("refused", auth.Bytes{}, nil, ""), "a refused revalidate doesn't keep it")
		assert.True(t, r.close("live", auth.Bytes{}, nil, ""), "revalidated: still remembered")
	})
}

func TestUsageRecordWire(t *testing.T) {
	ts := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	session, err := json.Marshal(usage.Record{Type: usage.Usage, SessionID: "s1", KID: "k1",
		Metrics: map[string]int64{usage.IngressBytes: 1}, TS: ts})
	require.NoError(t, err)
	viewers, err := json.Marshal(usage.Record{Type: usage.Viewers, Run: "r1", KID: "k1",
		Metrics: map[string]int64{usage.EgressBytes: 2}, TS: ts})
	require.NoError(t, err)

	assert.JSONEq(t, `{"type":"usage","session_id":"s1","kid":"k1",`+
		`"metrics":{"gateway.ingress_bytes":1},"ts":"2026-10-05T00:00:00Z"}`, string(session), "no reason or run when empty")
	assert.JSONEq(t, `{"type":"viewers","run":"r1","kid":"k1",`+
		`"metrics":{"gateway.egress_bytes":2},"ts":"2026-10-05T00:00:00Z"}`, string(viewers), "no session_id")
}

// A session bound to a key after it ended, by a ticket granted as it
// closed, is not brought back into usage, until the relay forgets it.
func TestUsageReporter_OpenAfterClose(t *testing.T) {
	sink := &fakeUsageSink{}
	r := newTestUsage(t, sink)
	now := time.Now()
	r.now = func() time.Time { return now }

	r.close("s1", auth.Bytes{}, nil, "closed")
	r.open("s1", usageSession{kid: "k1", publishes: true})
	r.publishing("s1")

	assert.Empty(t, r.sessions, "an open after the close is ignored")
	require.NoError(t, r.meter.Flush(context.Background()))
	assert.Empty(t, sink.received(), "no open is sent")

	now = now.Add(sessionGrace + time.Second)
	r.forgetStale()
	assert.Empty(t, r.closed, "a closed session is forgotten as a stale one is")
}

// TestUsageReporter_ViewersAreTotalled pins that viewer sessions cost
// the receiver one record per key, however many there are: no opens or
// closes, and one running total that only grows.
func TestUsageReporter_ViewersAreTotalled(t *testing.T) {
	sink := &fakeUsageSink{}
	r := newTestUsage(t, sink)

	for _, id := range []string{"v1", "v2", "v3"} {
		r.open(id, viewerSession("k1"))
	}
	r.open("other", viewerSession("k2"))
	r.revalidated("v1", auth.Bytes{Sent: 100, Received: 10}, nil, true)
	r.revalidated("v2", auth.Bytes{Sent: 200, Received: 20}, nil, true)
	r.revalidated("other", auth.Bytes{Sent: 7}, nil, true)
	require.NoError(t, r.meter.Flush(context.Background()))

	first := sink.received()[0]
	require.Len(t, first, 2, "one record per key, not per viewer")
	byKID := map[string]usage.Record{}
	for _, rec := range first {
		byKID[rec.KID] = rec
		assert.Equal(t, usage.Viewers, rec.Type)
		assert.Empty(t, rec.SessionID)
		assert.NoError(t, rec.Validate())
	}
	assert.Equal(t, byKID["k1"].Run, byKID["k2"].Run, "every key's viewers carry the same run")
	assert.Equal(t, map[string]int64{"gateway.egress_bytes": 300, "gateway.ingress_bytes": 30}, byKID["k1"].Metrics)
	assert.Equal(t, int64(7), byKID["k2"].Metrics["gateway.egress_bytes"])

	t.Run("an unchanged total isn't sent again", func(t *testing.T) {
		require.NoError(t, r.meter.Flush(context.Background()))
		assert.Len(t, sink.received(), 1)
	})

	t.Run("the total grows by what each session moved since it was last counted", func(t *testing.T) {
		r.revalidated("v1", auth.Bytes{Sent: 150, Received: 10}, nil, true)               // +50
		r.revalidated("v1", auth.Bytes{Sent: 120, Received: 10}, nil, true)               // a late, lower report adds nothing
		assert.True(t, r.close("v2", auth.Bytes{Sent: 260, Received: 25}, nil, "closed")) // +60, +5
		r.revalidated("v2", auth.Bytes{Sent: 999}, nil, true)                             // after its close: ignored
		assert.True(t, r.close("v3", auth.Bytes{}, nil, "closed"))                        // moved nothing
		require.NoError(t, r.meter.Flush(context.Background()))

		batches := sink.received()
		require.Len(t, batches, 2)
		require.Len(t, batches[1], 1, "only the key whose total changed; no close records")
		assert.Equal(t, map[string]int64{"gateway.egress_bytes": 410, "gateway.ingress_bytes": 35}, batches[1][0].Metrics)
	})
}

func TestUsageReporter_ViewerTotalSurvivesAFailedSend(t *testing.T) {
	sink := &fakeUsageSink{statuses: []int{http.StatusServiceUnavailable, http.StatusOK}}
	r := newTestUsage(t, sink)
	r.open("v1", viewerSession("k1"))
	r.revalidated("v1", auth.Bytes{Sent: 100}, nil, true)

	assert.Error(t, r.meter.Flush(context.Background()))
	r.revalidated("v1", auth.Bytes{Sent: 160}, nil, true)
	require.NoError(t, r.meter.Flush(context.Background()))

	batches := sink.received()
	require.Len(t, batches, 2)
	require.Len(t, batches[1], 1, "one record, not the old total and the new")
	assert.Equal(t, int64(160), batches[1][0].Metrics["gateway.egress_bytes"])
}

func TestUsageReporter_PublishersStayPerSession(t *testing.T) {
	sink := &fakeUsageSink{}
	r := newTestUsage(t, sink)
	r.open("p1", usageSession{kid: "k1", publishes: true})
	r.open("p2", usageSession{kid: "k1", publishes: true})
	r.revalidated("p1", auth.Bytes{Received: 50}, nil, true)
	require.NoError(t, r.meter.Flush(context.Background()))

	var got []string
	for _, rec := range sink.received()[0] {
		got = append(got, string(rec.Type)+" "+rec.SessionID)
		assert.NoError(t, rec.Validate())
	}
	assert.ElementsMatch(t, []string{"session_open p1", "session_open p2", "usage p1"}, got)
}

// publishing turns a viewer into a publisher once: a session it doesn't know,
// or one that publishes already, gets no second open.
func TestUsageReporter_Publishing(t *testing.T) {
	sink := &fakeUsageSink{}
	r := newTestUsage(t, sink)
	r.open("v1", viewerSession("k1"))
	r.open("p1", usageSession{kid: "k1", publishes: true})

	r.publishing("v1")
	r.publishing("v1")
	r.publishing("p1")
	r.publishing("unknown")
	r.revalidated("v1", auth.Bytes{Received: 40}, nil, true)
	require.NoError(t, r.meter.Flush(context.Background()))

	var got []string
	for _, rec := range sink.received()[0] {
		got = append(got, string(rec.Type)+" "+rec.SessionID)
	}
	assert.ElementsMatch(t, []string{"session_open p1", "session_open v1", "usage v1"}, got)
}

func TestBytesSince(t *testing.T) {
	tests := map[string]struct {
		b, base auth.Bytes
		want    auth.Bytes
	}{
		"nothing counted": {b: auth.Bytes{Sent: 5, Received: 7}, want: auth.Bytes{Sent: 5, Received: 7}},
		"part counted":    {b: auth.Bytes{Sent: 5, Received: 7}, base: auth.Bytes{Sent: 2, Received: 7}, want: auth.Bytes{Sent: 3}},
		"a lower report":  {b: auth.Bytes{Sent: 1, Received: 9}, base: auth.Bytes{Sent: 4, Received: 2}, want: auth.Bytes{Received: 7}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, bytesSince(tt.b, tt.base))
		})
	}
}

// A publishing session's usage carries how long it kept broadcasts opted
// into each egress, in whole seconds, and is reported for it even when the
// session moved no bytes; its close carries the final time. A viewer's
// total carries none.
func TestUsageReporter_OptInSeconds(t *testing.T) {
	sink := &fakeUsageSink{}
	r := newTestUsage(t, sink)
	r.open("p1", usageSession{kid: "k1", publishes: true})
	r.open("v1", viewerSession("k1"))
	abr := func(d time.Duration) map[token.OptIn]time.Duration {
		return map[token.OptIn]time.Duration{token.OptInABR: d}
	}

	r.revalidated("p1", auth.Bytes{Received: 100}, abr(1500*time.Millisecond), true)
	r.revalidated("v1", auth.Bytes{Sent: 10}, abr(time.Hour), true)
	require.NoError(t, r.meter.Flush(context.Background()))
	first := byType(sink.received()[0])
	assert.Equal(t, map[string]int64{
		usage.IngressBytes: 100, usage.EgressBytes: 0, usage.OptInSeconds("abr"): 1,
	}, first[usage.Usage].Metrics, "whole seconds")
	assert.NotContains(t, first[usage.Viewers].Metrics, usage.OptInSeconds("abr"), "a viewer opts nothing in")

	r.open("p2", usageSession{kid: "k1", publishes: true})
	r.revalidated("p2", auth.Bytes{}, abr(400*time.Millisecond), true)
	require.NoError(t, r.meter.Flush(context.Background()))
	require.Len(t, sink.received(), 2)
	assert.NotContains(t, byType(sink.received()[1]), usage.Usage,
		"no bytes and under a second opted in is nothing to report")

	r.revalidated("p2", auth.Bytes{}, abr(2*time.Second), true)
	require.NoError(t, r.meter.Flush(context.Background()))
	require.Len(t, sink.received(), 3)
	assert.Equal(t, int64(2), byType(sink.received()[2])[usage.Usage].Metrics[usage.OptInSeconds("abr")],
		"opt-in time without bytes is reported")

	assert.True(t, r.close("p1", auth.Bytes{Received: 120}, map[token.OptIn]time.Duration{
		token.OptInABR: 3 * time.Second, token.OptInHLS: 500 * time.Millisecond,
	}, usage.ReasonClosed))
	require.NoError(t, r.meter.Flush(context.Background()))
	closed := byType(sink.received()[3])[usage.SessionClose]
	assert.Equal(t, map[string]int64{
		usage.IngressBytes: 120, usage.EgressBytes: 0, usage.OptInSeconds("abr"): 3,
	}, closed.Metrics, "under a second of HLS is left out")
	assert.NoError(t, closed.Validate())
}

// TestRelay_UsageLifecycle drives Server.admit, revalidated/lease, and
// reportEnd against a verifier + Server.Meter and checks the records sent.
func TestRelay_UsageLifecycle(t *testing.T) {
	k, err := token.GenerateKey("acme/app")
	require.NoError(t, err)
	rawSet, err := token.MarshalKeySet(k.Public())
	require.NoError(t, err)
	keysPath := filepath.Join(t.TempDir(), "keys.json")
	require.NoError(t, os.WriteFile(keysPath, rawSet, 0o600))
	v, err := auth.NewVerifier(auth.VerifierConfig{Keys: keysPath})
	require.NoError(t, err)

	signTok := func(g token.Grant) string {
		tok, err := token.Sign(k, g, time.Minute)
		require.NoError(t, err)
		return tok
	}

	t.Run("admit, revalidate, and close report session records", func(t *testing.T) {
		sink := &fakeUsageSink{}
		srv := &Server{Authorize: v.Authorize, Meter: newTestMeter(t, sink)}
		srv.usage = newUsageReporter(srv.Meter)
		jwt := signTok(token.Grant{Root: "/acme/app/", Publish: []string{"/live"}, Subscribe: []string{"/"}})
		req := auth.Request{ID: "s1", Event: auth.EventConnect, Query: url.Values{"jwt": {jwt}}.Encode()}

		_, err := srv.admit(context.Background(), req)
		require.NoError(t, err)
		srv.usage.revalidated("s1", auth.Bytes{Sent: 10, Received: 20}, nil, true)
		srv.reportEnd(context.Background(), req, moqt.SessionStats{BytesSent: 30, BytesReceived: 40}, nil, usage.ReasonClosed)
		require.NoError(t, srv.Meter.Flush(context.Background()))

		batches := sink.received()
		require.Len(t, batches, 1)
		var types []usage.Type
		for _, r := range batches[0] {
			types = append(types, r.Type)
			assert.Equal(t, k.ID, r.KID)
		}
		assert.Equal(t, []usage.Type{usage.SessionOpen, usage.SessionClose}, types, "the close supersedes the pending usage")
		assert.Equal(t, map[string]int64{usage.IngressBytes: 40, usage.EgressBytes: 30}, batches[0][1].Metrics,
			"what the relay received is ingress, what it sent egress")
		assert.Equal(t, "Bearer t", sink.gotAuth)
	})

	t.Run("late revalidate after end does not bring session back", func(t *testing.T) {
		sink := &fakeUsageSink{}
		srv := &Server{Authorize: v.Authorize, Meter: newTestMeter(t, sink)}
		srv.usage = newUsageReporter(srv.Meter)
		jwt := signTok(token.Grant{Root: "/acme/app/", Publish: []string{"/live"}})
		req := auth.Request{ID: "s1", Event: auth.EventConnect, Query: url.Values{"jwt": {jwt}}.Encode()}

		_, err := srv.admit(context.Background(), req)
		require.NoError(t, err)
		srv.reportEnd(context.Background(), req, moqt.SessionStats{BytesSent: 5}, nil, usage.ReasonClosed)
		srv.usage.revalidated("s1", auth.Bytes{Sent: 9}, nil, true)
		require.NoError(t, srv.Meter.Flush(context.Background()))

		var types []usage.Type
		for _, rec := range sink.received()[0] {
			types = append(types, rec.Type)
		}
		assert.Equal(t, []usage.Type{usage.SessionOpen, usage.SessionClose}, types)
		assert.False(t, srv.usage.close("s1", auth.Bytes{}, nil, ""), "the session isn't known again")
	})
}

// byType returns batch's records by type, the last of each.
func byType(batch []usage.Record) map[usage.Type]usage.Record {
	out := map[usage.Type]usage.Record{}
	for _, rec := range batch {
		out[rec.Type] = rec
	}
	return out
}
