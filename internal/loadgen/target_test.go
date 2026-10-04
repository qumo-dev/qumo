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
		jwt         string
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
			relay:       "moqt://relay.example.com:4433/acme",
			jwt:         "h.p.s",
			wantRelay:   "relay.example.com:4433",
			wantURL:     "moqt://relay.example.com:4433/acme?jwt=h.p.s",
			wantMetrics: "http://relay.example.com:4433/metrics",
		},
		"host:port with a credential": {
			relay:       "127.0.0.1:4433",
			jwt:         "h.p.s",
			wantRelay:   "127.0.0.1:4433",
			wantURL:     "moqt://127.0.0.1:4433?jwt=h.p.s",
			wantMetrics: "http://127.0.0.1:4433/metrics",
		},
		"a WebTransport URL": {relay: "https://relay.example.com:4433/acme", wantErr: "scheme"},
		"a query":            {relay: "moqt://relay.example.com:4433/acme?jwt=h.p.s", wantErr: "query"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := newTarget(tt.relay, tt.jwt, "", "/bench", "data", "", true, time.Second, time.Second)

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

// TestNewTarget_QueryNotQuoted verifies a --relay with a query is refused
// without quoting it, even one too malformed to parse: url.Parse's error would
// quote its whole input, credential included.
func TestNewTarget_QueryNotQuoted(t *testing.T) {
	_, err := newTarget("moqt://[bad/acme?jwt=h.p.s", "", "", "/bench", "data", "", true, time.Second, time.Second)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "jwt=h.p.s")
}
