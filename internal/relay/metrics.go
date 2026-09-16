package relay

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// metricSessionsActive tracks the number of currently active MoQT sessions
	// being served by Relay(). Replaces the active_connections field that was
	// previously tracked inside statusHandler.
	metricSessionsActive = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "qumo",
		Subsystem: "relay",
		Name:      "sessions_active",
		Help:      "Current number of active MoQT relay sessions.",
	})

	// metricPeersConnected tracks the number of active outbound relay peer
	// connections managed by maintainPeer.
	metricPeersConnected = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "qumo",
		Subsystem: "relay",
		Name:      "peers_connected",
		Help:      "Current number of outbound relay peer connections.",
	})

	// metricBroadcastsActive tracks the number of relay broadcast routes
	// currently registered in the TrackMux (including routes that are
	// draining after being replaced by a better route).
	metricBroadcastsActive = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "qumo",
		Subsystem: "relay",
		Name:      "broadcasts_active",
		Help:      "Current number of active relay broadcast routes.",
	})

	// metricSessionRTTMilliseconds tracks the smoothed RTT (in milliseconds) to
	// each MoQT session, labelled by remote address.
	metricSessionRTTMilliseconds = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "qumo",
			Subsystem: "relay",
			Name:      "session_rtt_ms",
			Help:      "Smoothed round-trip time to each MoQT session in milliseconds.",
		},
		[]string{"remote"},
	)

	// metricSessionEstimatedBitrate tracks the estimated available bandwidth
	// (in bits per second) for each MoQT session, labelled by remote address.
	metricSessionEstimatedBitrate = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "qumo",
			Subsystem: "relay",
			Name:      "session_estimated_bitrate_bps",
			Help:      "Estimated available bandwidth for each MoQT session in bits per second.",
		},
		[]string{"remote"},
	)

	// metricPeerDialAttempts counts outbound peer dial attempts, labelled by
	// peer address and result ("ok" or "error").
	metricPeerDialAttempts = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "qumo",
			Subsystem: "relay",
			Name:      "peer_dial_attempts_total",
			Help:      "Total number of outbound relay peer dial attempts.",
		},
		[]string{"peer", "result"},
	)

	// metricPeerGoawayReceived counts GOAWAY messages received from upstream
	// peer relays (a migration/drain hint). Labelled by whether a redirect URI
	// was provided. Route/subscription migration is the primary mechanism;
	// GOAWAY is an escape hatch (see issue #280).
	metricPeerGoawayReceived = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "qumo",
			Subsystem: "relay",
			Name:      "peer_goaway_received_total",
			Help:      "GOAWAY messages received from upstream relay peers, by whether a redirect URI was supplied.",
		},
		[]string{"redirect"},
	)

	// metricDialRetriesTotal counts outbound peer dial retries. Each retry
	// is a reconnection attempt after a previous dial or session failure,
	// labelled by the peer address.
	metricDialRetriesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "qumo",
			Subsystem: "relay",
			Name:      "dial_retries_total",
			Help:      "Total number of outbound peer dial retries.",
		},
		[]string{"peer"},
	)

	// metricRelayIngressBytesTotal counts bytes received from upstream publishers.
	metricRelayIngressBytesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "qumo",
			Subsystem: "relay",
			Name:      "ingress_bytes_total",
			Help:      "Total bytes received from publishers, keyed by node/broadcast_path/track_name.",
		},
		[]string{"track"},
	)

	// metricRelayEgressBytesTotal counts bytes written to downstream subscribers.
	metricRelayEgressBytesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "qumo",
			Subsystem: "relay",
			Name:      "egress_bytes_total",
			Help:      "Total bytes sent to subscribers, keyed by node/broadcast_path/track_name, including fan-out.",
		},
		[]string{"track"},
	)

	// metricRouteReplacements counts how many times an existing broadcast route
	// was replaced by a strictly better candidate. The reason label carries the
	// winning dimension from compareRoutes (alive, hops, bitrate, rtt).
	metricRouteReplacements = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "qumo",
			Subsystem: "relay",
			Name:      "route_replacements_total",
			Help:      "Total number of relay broadcast routes replaced by a better route, by winning dimension.",
		},
		[]string{"reason"},
	)

	// metricRouteRejections counts route candidates that were rejected because
	// they were not better than the existing route. The reason label carries the
	// specific rejection cause from compareRoutes.
	metricRouteRejections = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "qumo",
			Subsystem: "relay",
			Name:      "route_rejections_total",
			Help:      "Total number of relay route candidates rejected, by rejection reason.",
		},
		[]string{"reason"},
	)

	// metricRelayRoutesRetained gauges route-election losers currently held as
	// alternates, pending promotion when the active route's announcement ends.
	metricRelayRoutesRetained = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "qumo",
		Subsystem: "relay",
		Name:      "routes_retained",
		Help:      "Current number of route-election losers retained as alternates pending incumbent-end recovery.",
	})

	// metricRelayRoutePromotions counts retained alternates promoted to the
	// active route after the incumbent announcement ended.
	metricRelayRoutePromotions = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "qumo",
		Subsystem: "relay",
		Name:      "route_promotions_total",
		Help:      "Retained alternate routes promoted to active after the incumbent announcement ended.",
	})

	// metricConnSmoothedRTT tracks the QUIC-layer smoothed RTT (ms) for each
	// inbound native-QUIC connection, labelled by remote address.
	// Only populated when the underlying transport exposes ConnectionStats()
	// (i.e. native QUIC; WebTransport connections are skipped).
	metricConnSmoothedRTT = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "qumo",
			Subsystem: "relay",
			Name:      "conn_smoothed_rtt_ms",
			Help:      "QUIC-layer smoothed RTT for each inbound native-QUIC connection in milliseconds.",
		},
		[]string{"remote"},
	)

	// metricConnPacketLossRate tracks the cumulative packet loss rate
	// (PacketsLost / PacketsSent) for each inbound native-QUIC connection.
	metricConnPacketLossRate = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "qumo",
			Subsystem: "relay",
			Name:      "conn_packet_loss_rate",
			Help:      "Cumulative packet loss rate (lost/sent) for each inbound native-QUIC connection.",
		},
		[]string{"remote"},
	)

	// metricSubscribersActive tracks the number of currently active MoQT track
	// subscribers.
	metricSubscribersActive = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "qumo",
		Subsystem: "relay",
		Name:      "subscribers_active",
		Help:      "Current number of active MoQT track subscribers.",
	})

	metricTrackSubscriptionsActive = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "qumo",
			Subsystem: "relay",
			Name:      "track_subscriptions_active",
			Help:      "Current downstream subscriptions observed for each broadcast path and track.",
		},
		[]string{"path", "track"},
	)

	metricTrackDistributorReusesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "qumo",
			Subsystem: "relay",
			Name:      "track_distributor_reuses_total",
			Help:      "Total track subscription requests served by an existing relay distributor.",
		},
		[]string{"path", "track"},
	)

	metricTrackUpstreamRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "qumo",
			Subsystem: "relay",
			Name:      "track_upstream_requests_total",
			Help:      "Total upstream track subscription requests.",
		},
		[]string{"path", "track"},
	)

	metricTrackUpstreamRequestErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "qumo",
			Subsystem: "relay",
			Name:      "track_upstream_request_errors_total",
			Help:      "Total failed upstream track subscription requests.",
		},
		[]string{"path", "track"},
	)

	metricTrackUpstreamRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "qumo",
			Subsystem: "relay",
			Name:      "track_upstream_request_duration_seconds",
			Help:      "Duration of upstream track subscription requests.",
		},
		[]string{"path", "track"},
	)

	// metricSubscriberSkipsTotal counts how many times a subscriber was skipped
	// forward because it fell behind the ring buffer.
	metricSubscriberSkipsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "qumo",
		Subsystem: "relay",
		Name:      "subscriber_skips_total",
		Help:      "Total number of times subscribers were skipped forward due to falling behind.",
	})

	// metricBufferDepthGroups tracks the number of groups currently held in the
	// track's ring buffer, labelled by track name.
	metricBufferDepthGroups = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "qumo",
			Subsystem: "relay",
			Name:      "buffer_depth_groups",
			Help:      "Number of groups currently held in the track's ring buffer.",
		},
		[]string{"track"},
	)

	// metricGroupFillsInflight tracks the number of fill goroutines currently
	// running across all trackDistributors. The per-track limit is
	// MaxGroupFillsInFlight, so the total may exceed it when multiple tracks
	// are active simultaneously.
	metricGroupFillsInflight = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "qumo",
		Subsystem: "relay",
		Name:      "group_fills_inflight",
		Help:      "Current number of group fill goroutines in flight across all track distributors.",
	})

	// metricSessionRTTHistogram tracks the distribution of RTT for all MoQT sessions.
	metricSessionRTTHistogram = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "qumo",
			Subsystem: "relay",
			Name:      "session_rtt_seconds",
			Help:      "Distribution of RTT for MoQT sessions in seconds.",
			Buckets:   []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		},
		[]string{"remote"},
	)

	// metricGroupDeliveryHistogram tracks the time it takes to deliver a full group to a subscriber.
	metricGroupDeliveryHistogram = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "qumo",
			Subsystem: "relay",
			Name:      "group_delivery_seconds",
			Help:      "Time taken to deliver a complete group to a subscriber in seconds.",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"track"},
	)

	// metricSubscribeErrorsTotal counts how many times a MoQT subscription
	// request failed, labelled by error code.
	metricSubscribeErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "qumo",
			Subsystem: "relay",
			Name:      "subscribe_errors_total",
			Help:      "Total number of MoQT subscription errors.",
		},
		[]string{"code"},
	)
)
