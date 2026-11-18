package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/qumo-dev/qumo/token"
	"github.com/qumo-dev/qumo/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func viewerSession(kid string) usageSession {
	return usageSession{kid: kid}
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
	r.revalidated("v1", Bytes{Sent: 100, Received: 10}, nil, true)
	r.revalidated("v2", Bytes{Sent: 200, Received: 20}, nil, true)
	r.revalidated("other", Bytes{Sent: 7}, nil, true)
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
		r.revalidated("v1", Bytes{Sent: 150, Received: 10}, nil, true)               // +50
		r.revalidated("v1", Bytes{Sent: 120, Received: 10}, nil, true)               // a late, lower report adds nothing
		assert.True(t, r.close("v2", Bytes{Sent: 260, Received: 25}, nil, "closed")) // +60, +5
		r.revalidated("v2", Bytes{Sent: 999}, nil, true)                             // after its close: ignored
		assert.True(t, r.close("v3", Bytes{}, nil, "closed"))                        // moved nothing
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
	r.revalidated("v1", Bytes{Sent: 100}, nil, true)

	assert.Error(t, r.meter.Flush(context.Background()))
	r.revalidated("v1", Bytes{Sent: 160}, nil, true)
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
	r.revalidated("p1", Bytes{Received: 50}, nil, true)
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
	r.revalidated("v1", Bytes{Received: 40}, nil, true)
	require.NoError(t, r.meter.Flush(context.Background()))

	var got []string
	for _, rec := range sink.received()[0] {
		got = append(got, string(rec.Type)+" "+rec.SessionID)
	}
	assert.ElementsMatch(t, []string{"session_open p1", "session_open v1", "usage v1"}, got)
}

func TestBytes_Since(t *testing.T) {
	tests := map[string]struct {
		b, base Bytes
		want    Bytes
	}{
		"nothing counted": {b: Bytes{Sent: 5, Received: 7}, want: Bytes{Sent: 5, Received: 7}},
		"part counted":    {b: Bytes{Sent: 5, Received: 7}, base: Bytes{Sent: 2, Received: 7}, want: Bytes{Sent: 3}},
		"a lower report":  {b: Bytes{Sent: 1, Received: 9}, base: Bytes{Sent: 4, Received: 2}, want: Bytes{Received: 7}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.b.since(tt.base))
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

	r.revalidated("p1", Bytes{Received: 100}, abr(1500*time.Millisecond), true)
	r.revalidated("v1", Bytes{Sent: 10}, abr(time.Hour), true)
	require.NoError(t, r.meter.Flush(context.Background()))
	first := byType(sink.received()[0])
	assert.Equal(t, map[string]int64{
		usage.IngressBytes: 100, usage.EgressBytes: 0, usage.OptInSeconds("abr"): 1,
	}, first[usage.Usage].Metrics, "whole seconds")
	assert.NotContains(t, first[usage.Viewers].Metrics, usage.OptInSeconds("abr"), "a viewer opts nothing in")

	r.open("p2", usageSession{kid: "k1", publishes: true})
	r.revalidated("p2", Bytes{}, abr(400*time.Millisecond), true)
	require.NoError(t, r.meter.Flush(context.Background()))
	require.Len(t, sink.received(), 2)
	assert.NotContains(t, byType(sink.received()[1]), usage.Usage,
		"no bytes and under a second opted in is nothing to report")

	r.revalidated("p2", Bytes{}, abr(2*time.Second), true)
	require.NoError(t, r.meter.Flush(context.Background()))
	require.Len(t, sink.received(), 3)
	assert.Equal(t, int64(2), byType(sink.received()[2])[usage.Usage].Metrics[usage.OptInSeconds("abr")],
		"opt-in time without bytes is reported")

	assert.True(t, r.close("p1", Bytes{Received: 120}, map[token.OptIn]time.Duration{
		token.OptInABR: 3 * time.Second, token.OptInHLS: 500 * time.Millisecond,
	}, usage.ReasonClosed))
	require.NoError(t, r.meter.Flush(context.Background()))
	closed := byType(sink.received()[3])[usage.SessionClose]
	assert.Equal(t, map[string]int64{
		usage.IngressBytes: 120, usage.EgressBytes: 0, usage.OptInSeconds("abr"): 3,
	}, closed.Metrics, "under a second of HLS is left out")
	assert.NoError(t, closed.Validate())
}

// byType returns batch's records by type, the last of each.
func byType(batch []usage.Record) map[usage.Type]usage.Record {
	out := map[usage.Type]usage.Record{}
	for _, rec := range batch {
		out[rec.Type] = rec
	}
	return out
}
