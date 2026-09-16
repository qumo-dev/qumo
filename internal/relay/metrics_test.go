package relay

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/client_golang/prometheus/testutil/promlint"
	"github.com/stretchr/testify/assert"
)

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

	// Verify that counters can be incremented and read
	metricRouteReplacements.WithLabelValues("alive").Inc()
	assert.Equal(t, 1.0, testutil.ToFloat64(metricRouteReplacements.WithLabelValues("alive")))

	metricSubscriberSkipsTotal.Add(5)
	assert.Equal(t, 5.0, testutil.ToFloat64(metricSubscriberSkipsTotal))

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

	// Verify counter vectors
	metricPeerDialAttempts.WithLabelValues("peer1", "ok").Inc()
	assert.Equal(t, 1.0, testutil.ToFloat64(metricPeerDialAttempts.WithLabelValues("peer1", "ok")))

	metricRelayIngressBytesTotal.WithLabelValues("track1").Add(1024)
	assert.Equal(t, 1024.0, testutil.ToFloat64(metricRelayIngressBytesTotal.WithLabelValues("track1")))

	metricRelayEgressBytesTotal.WithLabelValues("track1").Add(2048)
	assert.Equal(t, 2048.0, testutil.ToFloat64(metricRelayEgressBytesTotal.WithLabelValues("track1")))

	metricRouteRejections.WithLabelValues("not_better").Inc()
	assert.Equal(t, 1.0, testutil.ToFloat64(metricRouteRejections.WithLabelValues("not_better")))

	metricSubscribeErrorsTotal.WithLabelValues("not_found").Inc()
	assert.Equal(t, 1.0, testutil.ToFloat64(metricSubscribeErrorsTotal.WithLabelValues("not_found")))

	metricTrackCacheHitsTotal.WithLabelValues("/live/camera", "video").Inc()
	assert.Equal(t, 1.0, testutil.ToFloat64(metricTrackCacheHitsTotal.WithLabelValues("/live/camera", "video")))

	metricTrackCacheMissesTotal.WithLabelValues("/live/camera", "video").Inc()
	assert.Equal(t, 1.0, testutil.ToFloat64(metricTrackCacheMissesTotal.WithLabelValues("/live/camera", "video")))

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
		metricTrackCacheHitsTotal,
		metricTrackCacheMissesTotal,
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
