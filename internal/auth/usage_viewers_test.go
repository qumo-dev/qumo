package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func viewerSession(kid string) usageSession {
	return usageSession{kid: kid, role: roleSubscribe}
}

// TestUsageReporter_ViewersAreTotalled pins that subscribe-only sessions cost
// the receiver one record per key, however many there are: no opens or
// closes, and one running total that only grows.
func TestUsageReporter_ViewersAreTotalled(t *testing.T) {
	sink := &fakeUsageSink{}
	r := newTestUsage(t, sink)

	for _, id := range []string{"v1", "v2", "v3"} {
		r.open(id, viewerSession("k1"))
	}
	r.open("other", viewerSession("k2"))
	r.revalidated("v1", Bytes{Sent: 100, Received: 10}, true)
	r.revalidated("v2", Bytes{Sent: 200, Received: 20}, true)
	r.revalidated("other", Bytes{Sent: 7}, true)
	require.NoError(t, r.flush(context.Background()))

	first := sink.received()[0]
	require.Len(t, first, 2, "one record per key, not per viewer")
	byKID := map[string]usageRecord{}
	for _, rec := range first {
		byKID[rec.KID] = rec
		assert.Equal(t, recordUsage, rec.Type)
		assert.Equal(t, roleSubscribe, rec.Role)
		assert.Equal(t, "viewers."+r.runID+"."+rec.KID, rec.SessionID)
	}
	assert.Equal(t, map[string]int64{"gateway.egress_bytes": 300, "gateway.ingress_bytes": 30}, byKID["k1"].Metrics)
	assert.Equal(t, int64(7), byKID["k2"].Metrics["gateway.egress_bytes"])

	t.Run("an unchanged total isn't sent again", func(t *testing.T) {
		require.NoError(t, r.flush(context.Background()))
		assert.Len(t, sink.received(), 1)
	})

	t.Run("the total grows by what each session moved since it was last counted", func(t *testing.T) {
		r.revalidated("v1", Bytes{Sent: 150, Received: 10}, true)               // +50
		r.revalidated("v1", Bytes{Sent: 120, Received: 10}, true)               // a late, lower report adds nothing
		assert.True(t, r.close("v2", Bytes{Sent: 260, Received: 25}, "closed")) // +60, +5
		r.revalidated("v2", Bytes{Sent: 999}, true)                             // after its close: ignored
		assert.True(t, r.close("v3", Bytes{}, "closed"))                        // moved nothing
		require.NoError(t, r.flush(context.Background()))

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
	r.revalidated("v1", Bytes{Sent: 100}, true)

	assert.Error(t, r.flush(context.Background()))
	r.revalidated("v1", Bytes{Sent: 160}, true)
	require.NoError(t, r.flush(context.Background()))

	batches := sink.received()
	require.Len(t, batches, 2)
	require.Len(t, batches[1], 1, "one record, not the old total and the new")
	assert.Equal(t, int64(160), batches[1][0].Metrics["gateway.egress_bytes"])
}

func TestUsageReporter_PublishersStayPerSession(t *testing.T) {
	sink := &fakeUsageSink{}
	r := newTestUsage(t, sink)
	r.open("p1", usageSession{kid: "k1", role: rolePublish})
	r.open("b1", usageSession{kid: "k1", role: roleBoth})
	r.revalidated("p1", Bytes{Received: 50}, true)
	require.NoError(t, r.flush(context.Background()))

	var got []string
	for _, rec := range sink.received()[0] {
		got = append(got, rec.Type+" "+rec.SessionID)
		assert.False(t, strings.HasPrefix(rec.SessionID, "viewers."))
	}
	assert.ElementsMatch(t, []string{"session_open p1", "session_open b1", "usage p1"}, got)
}

func TestUsageReporter_EachRunHasItsOwnTotals(t *testing.T) {
	a, b := newTestUsage(t, &fakeUsageSink{}), newTestUsage(t, &fakeUsageSink{})
	assert.NotEqual(t, a.viewerSessionID("k1"), b.viewerSessionID("k1"),
		"a restarted relay starts new totals rather than lowering old ones")
}
