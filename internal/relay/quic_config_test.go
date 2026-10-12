package relay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/envconfig"
)

// clearQUICEnv empties every QUIC_* setting for the test, so one left in the
// developer's or the runner's environment cannot reach it.
func clearQUICEnv(tb testing.TB) {
	tb.Helper()
	for _, key := range envconfig.QUICKeys() {
		tb.Setenv(key, "")
	}
}

func TestQUICConfigFromEnv_Defaults(t *testing.T) {
	clearQUICEnv(t)

	cfg, err := quicConfigFromEnv()

	require.NoError(t, err)
	assert.True(t, cfg.Allow0RTT)
	assert.True(t, cfg.EnableDatagrams)
	assert.True(t, cfg.EnableStreamResetPartialDelivery)
	assert.Equal(t, 10*time.Second, cfg.KeepAlivePeriod)
	assert.Equal(t, 60*time.Second, cfg.MaxIdleTimeout)
	// The stream limits stay quic-go's: they are the relay's backpressure.
	assert.Zero(t, cfg.MaxIncomingStreams)
	assert.Zero(t, cfg.MaxIncomingUniStreams)
}

func TestQUICConfigFromEnv_Settings(t *testing.T) {
	clearQUICEnv(t)
	t.Setenv("QUIC_INITIAL_PACKET_SIZE", "1200")
	t.Setenv("QUIC_IDLE_TIMEOUT", "2m")

	cfg, err := quicConfigFromEnv()

	require.NoError(t, err)
	assert.Equal(t, uint16(1200), cfg.InitialPacketSize)
	assert.Equal(t, 2*time.Minute, cfg.MaxIdleTimeout)
	assert.Equal(t, 10*time.Second, cfg.KeepAlivePeriod, "the relay's default is kept")
	assert.True(t, cfg.Allow0RTT, "what the relay fixes is kept")
}
