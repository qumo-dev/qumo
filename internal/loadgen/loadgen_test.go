package loadgen

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sampleExposition = `# HELP go_goroutines Number of goroutines that currently exist.
# TYPE go_goroutines gauge
go_goroutines 4213
# HELP process_resident_memory_bytes Resident memory size in bytes.
# TYPE process_resident_memory_bytes gauge
process_resident_memory_bytes 1.34217728e+08
# HELP qumo_relay_sessions_active Active sessions.
# TYPE qumo_relay_sessions_active gauge
qumo_relay_sessions_active 4000
# a labelled metric must be ignored (name has braces, not an exact match)
qumo_relay_egress_bytes_total{track="data"} 999
`

func TestParseMetrics(t *testing.T) {
	tests := map[string]struct {
		text     string
		wantErr  bool
		goros    float64
		rss      float64
		sessions float64
	}{
		"full exposition": {
			text: sampleExposition, goros: 4213, rss: 1.34217728e+08, sessions: 4000,
		},
		"only goroutines present": {
			text: "go_goroutines 12\n", goros: 12,
		},
		"no expected metrics": {
			text: "# just a comment\nunrelated_metric 5\n", wantErr: true,
		},
		"malformed value": {
			text: "go_goroutines not-a-number\n", wantErr: true,
		},
		"empty": {
			text: "", wantErr: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			snap, err := parseMetrics(tt.text)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.goros, snap.goroutines)
			assert.Equal(t, tt.rss, snap.rssBytes)
			assert.Equal(t, tt.sessions, snap.sessionsActive)
		})
	}
}

func TestFetchMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(sampleExposition))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second}

	t.Run("success", func(t *testing.T) {
		snap, err := fetchMetrics(ctx, client, srv.URL+"/metrics")
		require.NoError(t, err)
		assert.Equal(t, float64(4213), snap.goroutines)
		assert.Equal(t, float64(4000), snap.sessionsActive)
	})

	t.Run("non-200 is an error", func(t *testing.T) {
		_, err := fetchMetrics(ctx, client, srv.URL+"/nope")
		assert.Error(t, err)
	})
}

func TestVerdictFor(t *testing.T) {
	tests := map[string]struct {
		receiving int
		sessions  int
		want      string
	}{
		"all receiving":          {receiving: 1000, sessions: 1000, want: "HOLDS"},
		"exactly 99pct boundary": {receiving: 990, sessions: 1000, want: "HOLDS"},
		"just below 99pct":       {receiving: 989, sessions: 1000, want: "CANNOT-HOLD"},
		"half":                   {receiving: 500, sessions: 1000, want: "CANNOT-HOLD"},
		"zero sessions":          {receiving: 0, sessions: 0, want: "HOLDS"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, verdictFor(tt.receiving, tt.sessions))
		})
	}
}

func TestBindCommon(t *testing.T) {
	dir := t.TempDir()
	ca := filepath.Join(dir, "cert.pem")
	require.NoError(t, os.WriteFile(ca, selfSignedCertPEM(t), 0o600))

	t.Run("ca or insecure required", func(t *testing.T) {
		fs := newTestFlagSet(t)
		finalize := bindCommon(fs)
		require.NoError(t, fs.Parse([]string{"--relay", "host:9"}))
		_, err := finalize()
		assert.Error(t, err)
	})

	t.Run("insecure satisfies without ca", func(t *testing.T) {
		fs := newTestFlagSet(t)
		finalize := bindCommon(fs)
		require.NoError(t, fs.Parse([]string{"--relay", "host:9", "--insecure"}))
		target, err := finalize()
		require.NoError(t, err)
		assert.True(t, target.tls.InsecureSkipVerify, "--insecure skips verification without a --ca")
		assert.Nil(t, target.tls.RootCAs)
	})

	t.Run("derives metrics url from relay", func(t *testing.T) {
		fs := newTestFlagSet(t)
		finalize := bindCommon(fs)
		require.NoError(t, fs.Parse([]string{"--relay", "example:4433", "--ca", ca}))
		target, err := finalize()
		require.NoError(t, err)
		assert.Equal(t, "http://example:4433/metrics", target.metrics)
		assert.Equal(t, "example:4433", target.relay)
	})

	t.Run("explicit metrics url wins", func(t *testing.T) {
		fs := newTestFlagSet(t)
		finalize := bindCommon(fs)
		require.NoError(t, fs.Parse([]string{"--relay", "example:4433", "--ca", ca, "--metrics", "http://mon:9100/metrics"}))
		target, err := finalize()
		require.NoError(t, err)
		assert.Equal(t, "http://mon:9100/metrics", target.metrics)
	})
}

func TestEmitJSONL(t *testing.T) {
	dir := t.TempDir()
	res := carryResult{connected: 950, receiving: 940, relayGoros: 15000, relayRSSMB: 120, perSessionKB: 129.3, launched: 1000}
	require.NoError(t, emitJSONL(dir, 1000, res))
	require.NoError(t, emitJSONL(dir, 500, carryResult{connected: 500, receiving: 500, launched: 500, rampPerSec: 250}))

	data, err := os.ReadFile(filepath.Join(dir, "results.jsonl"))
	require.NoError(t, err)
	lines := 0
	dec := json.NewDecoder(bytes.NewReader(data))
	var first jsonlRecord
	for {
		var rec jsonlRecord
		if err := dec.Decode(&rec); err != nil {
			break
		}
		if lines == 0 {
			first = rec
		}
		lines++
	}
	assert.Equal(t, 2, lines, "each call appends one record")
	assert.Equal(t, "capacity", first.Group)
	assert.Equal(t, 1000, first.Sessions)
	assert.Equal(t, "CANNOT-HOLD", first.Verdict) // 940 < 990
	assert.Equal(t, 15000, first.Goros)
}

func TestDialWithRetry_CancelledContext(t *testing.T) {
	// A pre-cancelled context should cause dialWithRetry to return promptly
	// with the context error, without hanging or retrying indefinitely.
	target := dialTarget{relay: "127.0.0.1:1"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	start := time.Now()
	sess, err := dialWithRetry(ctx, target)
	elapsed := time.Since(start)

	assert.Nil(t, sess, "session should be nil on cancelled context")
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, elapsed, 500*time.Millisecond, "should return promptly on cancelled context")
}

func TestDialWithRetry_TimeoutContext(t *testing.T) {
	// A context that expires quickly should cause dialWithRetry to return
	// with deadline exceeded after attempting (and failing) to dial.
	// Note: this exercises the cancellation path of the retry loop, not the
	// exponential backoff formula itself (which requires a longer-lived
	// context and a real relay, or extracted delay helpers).
	target := dialTarget{relay: "127.0.0.1:1"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	start := time.Now()
	sess, err := dialWithRetry(ctx, target)
	elapsed := time.Since(start)

	assert.Nil(t, sess, "session should be nil after timeout")
	assert.ErrorIs(t, err, context.DeadlineExceeded, "should return deadline exceeded")
	// Should complete within a generous bound (allowing for one failed dial + sleep).
	assert.Less(t, elapsed, 2*time.Second, "should return within timeout + slack")
}

func TestRun_Dispatch(t *testing.T) {
	t.Run("no args errors", func(t *testing.T) {
		assert.Error(t, Run(nil))
	})
	t.Run("unknown subcommand errors", func(t *testing.T) {
		assert.Error(t, Run([]string{"frobnicate"}))
	})
	t.Run("help is not an error", func(t *testing.T) {
		assert.NoError(t, Run([]string{"help"}))
	})
	t.Run("subscribe without N errors", func(t *testing.T) {
		assert.Error(t, Run([]string{"subscribe", "--ca", "/nonexistent"}))
	})
	t.Run("subscribe with non-numeric N errors", func(t *testing.T) {
		assert.Error(t, Run([]string{"subscribe", "notanumber"}))
	})
}

// ---- helpers ----

func newTestFlagSet(tb testing.TB) *flag.FlagSet {
	tb.Helper()
	return flag.NewFlagSet("test", flag.ContinueOnError)
}

func selfSignedCertPEM(tb testing.TB) []byte {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(tb, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(tb, err)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(tb, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
