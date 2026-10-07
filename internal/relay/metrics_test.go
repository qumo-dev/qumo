package relay

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/client_golang/prometheus/testutil/promlint"
	"github.com/stretchr/testify/assert"
)

// counterDelta increments c exactly once via inc and returns the observed
// delta. Prometheus collectors are process-global and counters accumulate, so
// absolute counter values are not stable across `go test -count` repetitions
// or increments left behind by other tests in the same binary; only the delta
// around a single increment is a stable assertion target.
func counterDelta(tb testing.TB, inc func(), c prometheus.Collector) float64 {
	tb.Helper()
	before := testutil.ToFloat64(c)
	inc()
	return testutil.ToFloat64(c) - before
}

func TestMetrics_Initialization(t *testing.T) {
	// Verify that gauges can be set and read
	metricSessionsActive.Set(42.0)
	assert.Equal(t, 42.0, testutil.ToFloat64(metricSessionsActive))

	metricPeersConnected.Set(10.0)
	assert.Equal(t, 10.0, testutil.ToFloat64(metricPeersConnected))

	metricBroadcastsActive.Set(5.0)
	assert.Equal(t, 5.0, testutil.ToFloat64(metricBroadcastsActive))

	metricSubscribersActive.Set(25.0)
	assert.Equal(t, 25.0, testutil.ToFloat64(metricSubscribersActive))

	metricTrackSubscriptionsActive.WithLabelValues("/live/camera", "video").Set(2.0)
	assert.Equal(t, 2.0, testutil.ToFloat64(metricTrackSubscriptionsActive.WithLabelValues("/live/camera", "video")))

	metricGroupFillsInflight.Set(3.0)
	assert.Equal(t, 3.0, testutil.ToFloat64(metricGroupFillsInflight))

	// Verify that counters can be incremented and read. Counters are
	// process-global and accumulate, so assertions are deltas around a single
	// increment — absolute values shift with -count=N repetitions and with
	// increments left by other tests in the same binary.
	routeReplacementsAlive := metricRouteReplacements.WithLabelValues("alive")
	assert.Equal(t, 1.0, counterDelta(t, routeReplacementsAlive.Inc, routeReplacementsAlive))

	assert.Equal(t, 5.0, counterDelta(t, func() { metricSubscriberSkipsTotal.Add(5) }, metricSubscriberSkipsTotal))

	// Verify gauge vectors
	metricSessionRTTMilliseconds.WithLabelValues("192.168.1.1").Set(15.5)
	assert.Equal(t, 15.5, testutil.ToFloat64(metricSessionRTTMilliseconds.WithLabelValues("192.168.1.1")))

	metricSessionEstimatedBitrate.WithLabelValues("10.0.0.1").Set(5000000.0)
	assert.Equal(t, 5000000.0, testutil.ToFloat64(metricSessionEstimatedBitrate.WithLabelValues("10.0.0.1")))

	metricConnSmoothedRTT.WithLabelValues("127.0.0.1").Set(2.5)
	assert.Equal(t, 2.5, testutil.ToFloat64(metricConnSmoothedRTT.WithLabelValues("127.0.0.1")))

	metricConnPacketLossRate.WithLabelValues("127.0.0.1").Set(0.01)
	assert.Equal(t, 0.01, testutil.ToFloat64(metricConnPacketLossRate.WithLabelValues("127.0.0.1")))

	metricBufferDepthGroups.WithLabelValues("track1").Set(10.0)
	assert.Equal(t, 10.0, testutil.ToFloat64(metricBufferDepthGroups.WithLabelValues("track1")))

	// Verify counter vectors (delta-based — see the counters note above).
	dialAttemptsOK := metricPeerDialAttempts.WithLabelValues("peer1", "ok")
	assert.Equal(t, 1.0, counterDelta(t, dialAttemptsOK.Inc, dialAttemptsOK))
	ingressTrack1 := metricRelayIngressBytesTotal.WithLabelValues("track1")
	assert.Equal(t, 1024.0, counterDelta(t, func() { ingressTrack1.Add(1024) }, ingressTrack1))
	egressTrack1 := metricRelayEgressBytesTotal.WithLabelValues("track1")
	assert.Equal(t, 2048.0, counterDelta(t, func() { egressTrack1.Add(2048) }, egressTrack1))
	routeRejectionsNotBetter := metricRouteRejections.WithLabelValues("not_better")
	assert.Equal(t, 1.0, counterDelta(t, routeRejectionsNotBetter.Inc, routeRejectionsNotBetter))
	subscribeErrorsNotFound := metricSubscribeErrorsTotal.WithLabelValues("not_found")
	assert.Equal(t, 1.0, counterDelta(t, subscribeErrorsNotFound.Inc, subscribeErrorsNotFound))
	distributorReuses := metricTrackDistributorReusesTotal.WithLabelValues("/live/camera", "video")
	assert.Equal(t, 1.0, counterDelta(t, distributorReuses.Inc, distributorReuses))
	upstreamRequests := metricTrackUpstreamRequestsTotal.WithLabelValues("/live/camera", "video")
	assert.Equal(t, 1.0, counterDelta(t, upstreamRequests.Inc, upstreamRequests))
	upstreamRequestErrors := metricTrackUpstreamRequestErrorsTotal.WithLabelValues("/live/camera", "video")
	assert.Equal(t, 1.0, counterDelta(t, upstreamRequestErrors.Inc, upstreamRequestErrors))

	metricTrackUpstreamRequestDuration.WithLabelValues("/live/camera", "video").Observe(0.1)
	assert.Equal(t, 1, testutil.CollectAndCount(metricTrackUpstreamRequestDuration))

	// Verify histogram vectors
	metricSessionRTTHistogram.Reset()
	histRTT := metricSessionRTTHistogram.WithLabelValues("test_client")
	histRTT.Observe(0.05)
	assert.Equal(t, 1, testutil.CollectAndCount(metricSessionRTTHistogram))

	metricGroupDeliveryHistogram.Reset()
	histDelivery := metricGroupDeliveryHistogram.WithLabelValues("track1")
	histDelivery.Observe(0.1)
	assert.Equal(t, 1, testutil.CollectAndCount(metricGroupDeliveryHistogram))
}

func TestMetrics_Lint(t *testing.T) {
	metrics := []prometheus.Collector{
		metricSessionsActive,
		metricPeersConnected,
		metricBroadcastsActive,
		metricSessionRTTMilliseconds,
		metricSessionEstimatedBitrate,
		metricPeerDialAttempts,
		metricRelayIngressBytesTotal,
		metricRelayEgressBytesTotal,
		metricRouteReplacements,
		metricRouteRejections,
		metricConnSmoothedRTT,
		metricConnPacketLossRate,
		metricSubscribersActive,
		metricTrackSubscriptionsActive,
		metricTrackDistributorReusesTotal,
		metricTrackUpstreamRequestsTotal,
		metricTrackUpstreamRequestErrorsTotal,
		metricTrackUpstreamRequestDuration,
		metricSubscriberSkipsTotal,
		metricBufferDepthGroups,
		metricGroupFillsInflight,
		metricSessionRTTHistogram,
		metricGroupDeliveryHistogram,
		metricSubscribeErrorsTotal,
	}

	for _, m := range metrics {
		problems, err := testutil.CollectAndLint(m)
		assert.NoError(t, err)

		// Filter out known exceptions for existing metrics that use abbreviated units
		var filteredProblems []promlint.Problem
		for _, p := range problems {
			if m == metricSessionRTTMilliseconds && p.Text == "metric names should not contain abbreviated units" {
				continue
			}
			if m == metricConnSmoothedRTT && p.Text == "metric names should not contain abbreviated units" {
				continue
			}
			filteredProblems = append(filteredProblems, p)
		}

		assert.Empty(t, filteredProblems, "Metric should not have linting problems")
	}
}
