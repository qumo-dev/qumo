package trust

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	metricLastSuccess = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "qumo",
		Subsystem: "relay",
		Name:      "trust_last_success_seconds",
		Help:      "Unix time of the last successful trust snapshot poll (200 or 304).",
	})

	metricPollFailures = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "qumo",
		Subsystem: "relay",
		Name:      "trust_poll_failures_total",
		Help:      "Total number of failed trust snapshot polls.",
	})

	metricKeys = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "qumo",
		Subsystem: "relay",
		Name:      "trust_keys",
		Help:      "Number of usable signing keys in the current trust snapshot.",
	})
)
