package relay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQUICConfigFromEnv_Defaults(t *testing.T) {
	cfg, err := quicConfigFromEnv()

	require.NoError(t, err)
	assert.True(t, cfg.Allow0RTT)
	assert.True(t, cfg.EnableDatagrams)
	assert.True(t, cfg.EnableStreamResetPartialDelivery)
	assert.Equal(t, 10*time.Second, cfg.KeepAlivePeriod)
	assert.Equal(t, 60*time.Second, cfg.MaxIdleTimeout)
	// Zero leaves each of these to quic-go's own default.
	assert.Zero(t, cfg.InitialPacketSize)
	assert.False(t, cfg.DisablePathMTUDiscovery)
	assert.Zero(t, cfg.HandshakeIdleTimeout)
	assert.Zero(t, cfg.InitialStreamReceiveWindow)
	assert.Zero(t, cfg.MaxStreamReceiveWindow)
	assert.Zero(t, cfg.InitialConnectionReceiveWindow)
	assert.Zero(t, cfg.MaxConnectionReceiveWindow)
	// The stream limits stay quic-go's: they are the relay's backpressure.
	assert.Zero(t, cfg.MaxIncomingStreams)
	assert.Zero(t, cfg.MaxIncomingUniStreams)
}

func TestQUICConfigFromEnv_Settings(t *testing.T) {
	t.Setenv("RELAY_QUIC_INITIAL_PACKET_SIZE", "1200")
	t.Setenv("RELAY_QUIC_DISABLE_PMTUD", "true")
	t.Setenv("RELAY_QUIC_KEEPALIVE_PERIOD", "0")
	t.Setenv("RELAY_QUIC_MAX_IDLE_TIMEOUT", "2m")
	t.Setenv("RELAY_QUIC_HANDSHAKE_IDLE_TIMEOUT", "3s")
	t.Setenv("RELAY_QUIC_INITIAL_STREAM_RECEIVE_WINDOW", "1048576")
	t.Setenv("RELAY_QUIC_MAX_STREAM_RECEIVE_WINDOW", "2097152")
	t.Setenv("RELAY_QUIC_INITIAL_CONNECTION_RECEIVE_WINDOW", "3145728")
	t.Setenv("RELAY_QUIC_MAX_CONNECTION_RECEIVE_WINDOW", "4194304")

	cfg, err := quicConfigFromEnv()

	require.NoError(t, err)
	assert.Equal(t, uint16(1200), cfg.InitialPacketSize)
	assert.True(t, cfg.DisablePathMTUDiscovery)
	assert.Zero(t, cfg.KeepAlivePeriod)
	assert.Equal(t, 2*time.Minute, cfg.MaxIdleTimeout)
	assert.Equal(t, 3*time.Second, cfg.HandshakeIdleTimeout)
	assert.Equal(t, uint64(1048576), cfg.InitialStreamReceiveWindow)
	assert.Equal(t, uint64(2097152), cfg.MaxStreamReceiveWindow)
	assert.Equal(t, uint64(3145728), cfg.InitialConnectionReceiveWindow)
	assert.Equal(t, uint64(4194304), cfg.MaxConnectionReceiveWindow)
}

func TestQUICConfigFromEnv_InitialPacketSizeBounds(t *testing.T) {
	tests := map[string]struct {
		value   string
		want    uint16
		wantErr bool
	}{
		"smallest QUIC allows":  {value: "1200", want: 1200},
		"largest quic-go sends": {value: "1452", want: 1452},
		"one below the minimum": {value: "1199", wantErr: true},
		"one above the maximum": {value: "1453", wantErr: true},
		"zero":                  {value: "0", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("RELAY_QUIC_INITIAL_PACKET_SIZE", tt.value)

			cfg, err := quicConfigFromEnv()

			if tt.wantErr {
				assert.ErrorContains(t, err, "RELAY_QUIC_INITIAL_PACKET_SIZE")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.InitialPacketSize)
		})
	}
}

func TestQUICConfigFromEnv_InvalidValue(t *testing.T) {
	tests := map[string]struct {
		key   string
		value string
	}{
		"packet size not a number": {key: "RELAY_QUIC_INITIAL_PACKET_SIZE", value: "big"},
		"packet size negative":     {key: "RELAY_QUIC_INITIAL_PACKET_SIZE", value: "-1200"},
		"switch not a boolean":     {key: "RELAY_QUIC_DISABLE_PMTUD", value: "maybe"},
		"keep-alive without unit":  {key: "RELAY_QUIC_KEEPALIVE_PERIOD", value: "10"},
		"idle timeout negative":    {key: "RELAY_QUIC_MAX_IDLE_TIMEOUT", value: "-1s"},
		"handshake timeout words":  {key: "RELAY_QUIC_HANDSHAKE_IDLE_TIMEOUT", value: "soon"},
		"window with a unit":       {key: "RELAY_QUIC_MAX_STREAM_RECEIVE_WINDOW", value: "6MB"},
		"window negative":          {key: "RELAY_QUIC_INITIAL_CONNECTION_RECEIVE_WINDOW", value: "-1"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)

			_, err := quicConfigFromEnv()

			// The message names the variable, so the operator knows which one to fix.
			assert.ErrorContains(t, err, tt.key)
		})
	}
}
