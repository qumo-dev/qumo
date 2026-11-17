// Package certreload is what the commands that re-read their certificate
// files while they run share: the relay (CERT_FILE, KEY_FILE,
// PEER_CERT_FILE, PEER_KEY_FILE, CA_FILE) and qumo dns (PEER_CERT_FILE,
// PEER_KEY_FILE). Each command reads its files and decides what it takes;
// this package holds the interval, the loop, and how each certificate's
// readings are logged and counted.
package certreload

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Interval is how often the certificate files are read.
const Interval = time.Minute

// Run calls reload every Interval until ctx ends.
func Run(ctx context.Context, reload func()) {
	ticker := time.NewTicker(Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reload()
		}
	}
}

// SameChain reports whether a and b hold the same certificates; nil for
// none.
func SameChain(a, b *tls.Certificate) bool {
	if a == nil || b == nil {
		return a == b
	}
	return slices.EqualFunc(a.Certificate, b.Certificate, slices.Equal)
}

// Metrics are one command's certificate series, under its subsystem:
// qumo_<subsystem>_cert_reloads_total{cert,result} and
// qumo_<subsystem>_cert_not_after_seconds{cert}.
type Metrics struct {
	subsystem string
	reloads   *prometheus.CounterVec
	notAfter  *prometheus.GaugeVec
}

// NewMetrics registers the certificate series of subsystem (relay, dns) on
// reg.
func NewMetrics(reg prometheus.Registerer, subsystem string) *Metrics {
	m := &Metrics{
		subsystem: subsystem,
		reloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "qumo", Subsystem: subsystem, Name: "cert_reloads_total",
			Help: "Certificate files read anew, by certificate and result: ok (taken) or error (refused; the certificate in use is kept). Files that hold what is in use count nothing.",
		}, []string{"cert", "result"}),
		notAfter: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "qumo", Subsystem: subsystem, Name: "cert_not_after_seconds",
			Help: "When the certificate in use expires, in unix seconds, by certificate.",
		}, []string{"cert"}),
	}
	reg.MustRegister(m.reloads, m.notAfter)
	return m
}

// Cert logs and counts the readings of one certificate's files. Every
// reading that fails is counted, but logged only when its error differs
// from the one logged last: a file that stays broken, or a certificate that
// stays expired, logs once, not at every reading. A good reading clears
// that. Its methods are called from the one goroutine that reloads.
type Cert struct {
	log  *slog.Logger
	m    *Metrics
	name string // the cert label: public, peer
	// failure is the failure logged last, "" after a good reading.
	failure string
}

// NewCert returns the Cert of the certificate name, and records the expiry
// of inUse, the certificate in use (nil for none).
func NewCert(log *slog.Logger, m *Metrics, name string, inUse *tls.Certificate) *Cert {
	c := &Cert{log: log, m: m, name: name}
	c.recordNotAfter(inUse)
	return c
}

// Taken records files read anew and taken. cert is the certificate taken,
// whose Leaf is set (tls.LoadX509KeyPair sets it), or nil when what was
// taken holds no new certificate (the relay's CA alone).
func (c *Cert) Taken(cert *tls.Certificate) {
	c.failure = ""
	c.m.reloads.WithLabelValues(c.name, "ok").Inc()
	if cert == nil {
		return
	}
	c.recordNotAfter(cert)
	c.log.Info(c.m.subsystem+": certificate renewed; in use from now on", "cert", c.name,
		"subject", cert.Leaf.Subject.CommonName, "serial", fmt.Sprintf("%X", cert.Leaf.SerialNumber), "not_after", cert.Leaf.NotAfter)
}

// Unchanged records files that hold what is in use.
func (c *Cert) Unchanged() {
	if c.failure == "" {
		return
	}
	c.failure = ""
	c.log.Info(c.m.subsystem+": certificate files read again; they hold the certificate in use", "cert", c.name)
}

// Failed records files that could not be taken (settings: the settings
// that name them), the certificate in use being kept.
func (c *Cert) Failed(settings string, err error) {
	c.m.reloads.WithLabelValues(c.name, "error").Inc()
	failure := settings + ": " + err.Error()
	if failure == c.failure {
		return
	}
	c.failure = failure
	c.log.Error(c.m.subsystem+": certificate files not taken; keeping the certificate in use (logged again only if the error changes)",
		"cert", c.name, "settings", settings, "err", err)
}

// recordNotAfter sets the expiry gauge for cert; nil (no peer identity)
// leaves it unset.
func (c *Cert) recordNotAfter(cert *tls.Certificate) {
	if cert == nil {
		return
	}
	c.m.notAfter.WithLabelValues(c.name).Set(float64(cert.Leaf.NotAfter.Unix()))
}
