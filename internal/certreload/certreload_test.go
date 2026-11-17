package certreload

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/reqsign/reqsigntest"
)

// pair returns a certificate and key with its Leaf set, as
// tls.LoadX509KeyPair returns them.
func pair(tb testing.TB) *tls.Certificate {
	tb.Helper()
	p := reqsigntest.Pair(tb)
	leaf, err := x509.ParseCertificate(p.Certificate[0])
	require.NoError(tb, err)
	p.Leaf = leaf
	return &p
}

// A failure is logged when it is new, counted every time, and forgotten
// after a good reading: a file that stays broken logs once, not at every
// reading.
func TestCert_Failed(t *testing.T) {
	var logs bytes.Buffer
	m := NewMetrics(prometheus.NewRegistry(), "test")
	inUse := pair(t)
	c := NewCert(slog.New(slog.NewTextHandler(&logs, nil)), m, "peer", inUse)
	broken := errors.New("tls: private key does not match public key")
	expired := errors.New("x509: certificate has expired or is not yet valid")
	renewed := pair(t)

	steps := []struct {
		name       string
		read       func()
		wantErrors int // ERROR lines logged so far
		wantFailed float64
	}{
		{"a broken pair", func() { c.Failed("PEER_CERT_FILE, PEER_KEY_FILE", broken) }, 1, 1},
		{"the same, again", func() { c.Failed("PEER_CERT_FILE, PEER_KEY_FILE", broken) }, 1, 2},
		{"another error", func() { c.Failed("PEER_CERT_FILE, PEER_KEY_FILE", expired) }, 2, 3},
		{"the same error from other files", func() { c.Failed("CA_FILE", expired) }, 3, 4},
		{"back to the files in use", c.Unchanged, 3, 4},
		{"the last error, after a good reading", func() { c.Failed("CA_FILE", expired) }, 4, 5},
		{"a renewal taken", func() { c.Taken(renewed) }, 4, 5},
		{"the last error, after the renewal", func() { c.Failed("CA_FILE", expired) }, 5, 6},
	}
	for _, s := range steps {
		s.read()
		assert.Equal(t, s.wantErrors, strings.Count(logs.String(), "level=ERROR"), "ERROR lines after %s", s.name)
		assert.Equal(t, s.wantFailed, testutil.ToFloat64(m.reloads.WithLabelValues("peer", "error")), "failures counted after %s", s.name)
	}
	assert.Equal(t, 1, strings.Count(logs.String(), "they hold the certificate in use"), "the return to the files in use is logged once")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.reloads.WithLabelValues("peer", "ok")))
	assert.Equal(t, float64(renewed.Leaf.NotAfter.Unix()), testutil.ToFloat64(m.notAfter.WithLabelValues("peer")), "the expiry of the certificate in use")
}

// The expiry gauge holds the certificate in use from the start, and is left
// unset without one.
func TestNewCert(t *testing.T) {
	inUse := pair(t)
	tests := map[string]struct {
		inUse     *tls.Certificate
		wantCount int
	}{
		"a certificate": {inUse: inUse, wantCount: 1},
		"none":          {inUse: nil, wantCount: 0},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			m := NewMetrics(prometheus.NewRegistry(), "test")

			NewCert(slog.New(slog.DiscardHandler), m, "peer", tt.inUse)

			assert.Equal(t, tt.wantCount, testutil.CollectAndCount(m.notAfter))
			if tt.inUse != nil {
				assert.Equal(t, float64(inUse.Leaf.NotAfter.Unix()), testutil.ToFloat64(m.notAfter.WithLabelValues("peer")))
			}
		})
	}
}

// Run reloads once an Interval, and stops with its context.
func TestRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		var reloads atomic.Int32
		done := make(chan struct{})
		go func() {
			defer close(done)
			Run(ctx, func() { reloads.Add(1) })
		}()

		time.Sleep(Interval - time.Second)
		synctest.Wait()
		assert.Equal(t, int32(0), reloads.Load(), "not read before the interval")

		time.Sleep(time.Second)
		synctest.Wait()
		assert.Equal(t, int32(1), reloads.Load(), "read once the interval is up")

		time.Sleep(Interval)
		synctest.Wait()
		assert.Equal(t, int32(2), reloads.Load(), "and once every interval after")

		cancel()
		<-done
	})
}

// SameChain compares the certificates, not the keys or the parsed leaf.
func TestSameChain(t *testing.T) {
	a, b := pair(t), pair(t)
	aAgain := &tls.Certificate{Certificate: a.Certificate}
	tests := map[string]struct {
		a, b *tls.Certificate
		want bool
	}{
		"the same certificates": {a: a, b: aAgain, want: true},
		"other certificates":    {a: a, b: b, want: false},
		"one nil":               {a: a, b: nil, want: false},
		"both nil":              {a: nil, b: nil, want: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, SameChain(tt.a, tt.b))
		})
	}
}
