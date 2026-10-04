package loadgen

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewTarget_Relay(t *testing.T) {
	tests := map[string]struct {
		relay       string
		wantRelay   string
		wantURL     string
		wantMetrics string
		wantErr     bool
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
		"a WebTransport URL": {relay: "https://relay.example.com:4433/acme", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := newTarget(tt.relay, "", "/bench", "data", "", true, time.Second, time.Second)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantRelay, got.relay, "the relay that is logged has no query")
			assert.Equal(t, tt.wantURL, got.url)
			assert.Equal(t, tt.wantMetrics, got.metrics)
		})
	}
}
