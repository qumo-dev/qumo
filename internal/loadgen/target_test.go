package loadgen

import (
	"flag"
	"testing"
	"time"

	"github.com/qumo-dev/qumo/internal/envconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewTarget_Relay(t *testing.T) {
	tests := map[string]struct {
		relay       string
		wantRelay   string
		wantURL     string
		wantMetrics string
		wantErr     string
	}{
		"host:port": {
			relay:       "127.0.0.1:4433",
			wantRelay:   "127.0.0.1:4433",
			wantURL:     "moqt://127.0.0.1:4433",
			wantMetrics: "http://127.0.0.1:4433/metrics",
		},
		"a moqt URL with a credential": {
			relay:       "moqt://relay.example.com:4433/acme?jwt=h.p.s",
			wantRelay:   "relay.example.com:4433",
			wantURL:     "moqt://relay.example.com:4433/acme?jwt=h.p.s",
			wantMetrics: "http://relay.example.com:4433/metrics",
		},
		"a WebTransport URL": {relay: "https://relay.example.com:4433/acme", wantErr: "scheme"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := newTarget(tt.relay, "", "/bench", "data", "", true, nil)

			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantRelay, got.relay, "the relay that is logged has no credential")
			assert.Equal(t, tt.wantURL, got.url)
			assert.Equal(t, tt.wantMetrics, got.metrics)
		})
	}
}

// TestNewTarget_MalformedRelayHidesCredential verifies a --relay URL that
// doesn't parse is reported without quoting it: url.Parse's error would quote
// its whole input, credential included.
func TestNewTarget_MalformedRelayHidesCredential(t *testing.T) {
	_, err := newTarget("moqt://[bad/acme?jwt=h.p.s", "", "/bench", "data", "", true, nil)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "jwt=h.p.s")
}

func TestTargetQUICConfig(t *testing.T) {
	tests := map[string]struct {
		args          []string
		env           map[string]string
		wantIdle      time.Duration
		wantKeepAlive time.Duration
		wantPacket    uint16
		wantErr       string
	}{
		"flag defaults": {
			wantIdle: 30 * time.Second, wantKeepAlive: 5 * time.Second,
		},
		"the environment replaces the defaults": {
			env:      map[string]string{"QUIC_IDLE_TIMEOUT": "2m", "QUIC_KEEPALIVE": "20s", "QUIC_INITIAL_PACKET_SIZE": "1200"},
			wantIdle: 2 * time.Minute, wantKeepAlive: 20 * time.Second, wantPacket: 1200,
		},
		"a flag on the command line wins over the environment": {
			args:     []string{"--idle-timeout", "40s", "--keepalive", "8s"},
			env:      map[string]string{"QUIC_IDLE_TIMEOUT": "2m", "QUIC_KEEPALIVE": "20s"},
			wantIdle: 40 * time.Second, wantKeepAlive: 8 * time.Second,
		},
		"one flag passed, the other from the environment": {
			args:     []string{"--keepalive", "8s"},
			env:      map[string]string{"QUIC_IDLE_TIMEOUT": "2m", "QUIC_KEEPALIVE": "20s"},
			wantIdle: 2 * time.Minute, wantKeepAlive: 8 * time.Second,
		},
		"a refused setting": {
			env:     map[string]string{"QUIC_INITIAL_PACKET_SIZE": "900"},
			wantErr: "QUIC_INITIAL_PACKET_SIZE",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			for _, key := range envconfig.QUICKeys() {
				t.Setenv(key, "")
			}
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
			idle := fs.Duration("idle-timeout", 30*time.Second, "")
			keepalive := fs.Duration("keepalive", 5*time.Second, "")
			require.NoError(t, fs.Parse(tt.args))

			cfg, err := targetQUICConfig(fs, *idle, *keepalive)

			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantIdle, cfg.MaxIdleTimeout)
			assert.Equal(t, tt.wantKeepAlive, cfg.KeepAlivePeriod)
			assert.Equal(t, tt.wantPacket, cfg.InitialPacketSize)
			assert.True(t, cfg.EnableDatagrams)
		})
	}
}
