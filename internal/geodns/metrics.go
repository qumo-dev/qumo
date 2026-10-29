package geodns

import "github.com/prometheus/client_golang/prometheus"

// metrics are the server's Prometheus series, registered on one registry so
// that a test builds its own.
type metrics struct {
	queries       *prometheus.CounterVec
	located       *prometheus.CounterVec
	answers       *prometheus.CounterVec
	feedRounds    *prometheus.CounterVec
	feedUpdated   prometheus.Gauge
	geoRounds     *prometheus.CounterVec
	zoneRegions   prometheus.Gauge
	zoneAddresses prometheus.Gauge
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		queries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "qumo", Subsystem: "dns", Name: "queries_total",
			Help: "Queries answered, by response code.",
		}, []string{"rcode"}),
		located: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "qumo", Subsystem: "dns", Name: "client_locations_total",
			Help: "Address queries by whether the client was placed (located, unknown).",
		}, []string{"outcome"}),
		answers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "qumo", Subsystem: "dns", Name: "answers_total",
			Help: "Address queries answered from a zone, by the region answered (empty: no address of the type) and whether the client was placed (located, unknown).",
		}, []string{"region", "client"}),
		feedRounds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "qumo", Subsystem: "dns", Name: "feed_rounds_total",
			Help: "Feed reads, by outcome (updated, unchanged, not_ready, error).",
		}, []string{"outcome"}),
		feedUpdated: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "qumo", Subsystem: "dns", Name: "feed_last_success_timestamp_seconds",
			Help: "When the feed was last read successfully.",
		}),
		geoRounds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "qumo", Subsystem: "dns", Name: "geo_refresh_total",
			Help: "Location database refreshes, by outcome (updated, unchanged, error).",
		}, []string{"outcome"}),
		zoneRegions: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "qumo", Subsystem: "dns", Name: "zone_regions",
			Help: "Regions with an address in the zone served.",
		}),
		zoneAddresses: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "qumo", Subsystem: "dns", Name: "zone_addresses",
			Help: "Addresses in the zone served.",
		}),
	}
	reg.MustRegister(m.queries, m.located, m.answers, m.feedRounds, m.feedUpdated, m.geoRounds, m.zoneRegions, m.zoneAddresses)
	return m
}
