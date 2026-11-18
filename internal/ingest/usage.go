package ingest

import (
	"context"
	"crypto/rand"
	"math"
	"time"

	"github.com/qumo-dev/qumo/internal/meter"
	"github.com/qumo-dev/qumo/internal/optin"
	"github.com/qumo-dev/qumo/usage"
)

// usageInterval is how often a pushed stream's usage is reported while it
// runs, as a relay revalidates a session.
const usageInterval = 30 * time.Second

// meterStream reports the usage of sess, a stream pushed under the key kid,
// to m until the returned function is called with why it ended: an open now,
// its cumulative usage every usageInterval, and a close with its final
// totals. With no meter or no key (auth off) it reports nothing.
func meterStream(m *meter.Reporter, kid string, sess *Session) (end func(reason string)) {
	if m == nil || kid == "" {
		return func(string) {}
	}
	id := rand.Text()
	m.Open(id, kid)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(usageInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.Report(id, kid, streamMetrics(sess))
			}
		}
	}()
	return func(reason string) {
		cancel()
		<-done
		m.Close(id, kid, streamMetrics(sess), reason)
	}
}

// streamMetrics is a pushed stream's usage: the media it took in, and how
// long it kept its broadcast opted into each egress, in whole seconds.
func streamMetrics(sess *Session) map[string]int64 {
	mediaBytes, optIns := sess.Usage()
	m := map[string]int64{usage.IngestMediaBytes: int64(min(mediaBytes, math.MaxInt64))}
	optin.AddSeconds(m, optIns)
	return m
}
