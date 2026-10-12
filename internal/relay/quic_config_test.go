package relay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearQUICEnv empties every RELAY_QUIC_* setting for the test, so one left in
// the developer's or the runner's environment cannot reach it.
func clearQUICEnv(tb testing.TB) {
	tb.Helper()
	for _, key := range quicEnvKeys() {
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
	clearQUICEnv(t)
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
			clearQUICEnv(t)
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

func TestQUICConfigFromEnv_DisablePMTUDWords(t *testing.T) {
	tests := map[string]struct {
		value   string
		want    bool
		wantErr bool
	}{
		"true":             {value: "true", want: true},
		"one":              {value: "1", want: true},
		"yes in capitals":  {value: "YES", want: true},
		"on with spaces":   {value: " on ", want: true},
		"false":            {value: "false"},
		"zero":             {value: "0"},
		"no":               {value: "no"},
		"off":              {value: "off"},
		"only spaces":      {value: "  "},
		"not a switch":     {value: "maybe", wantErr: true},
		"a single letter":  {value: "t", wantErr: true},
		"a number above 1": {value: "2", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			clearQUICEnv(t)
			t.Setenv("RELAY_QUIC_DISABLE_PMTUD", tt.value)

			cfg, err := quicConfigFromEnv()

			if tt.wantErr {
				assert.ErrorContains(t, err, "RELAY_QUIC_DISABLE_PMTUD")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.DisablePathMTUDiscovery)
		})
	}
}

func TestQUICConfigFromEnv_TimeoutBounds(t *testing.T) {
	// The two timeouts refuse zero, which quic-go would read as "use my
	// default", and anything a peer would be told is no timeout at all.
	for _, key := range []string{"RELAY_QUIC_MAX_IDLE_TIMEOUT", "RELAY_QUIC_HANDSHAKE_IDLE_TIMEOUT"} {
		tests := map[string]struct {
			value   string
			wantErr bool
		}{
			"one millisecond":         {value: "1ms"},
			"zero":                    {value: "0", wantErr: true},
			"under a millisecond":     {value: "500us", wantErr: true},
			"one nanosecond short":    {value: "999999ns", wantErr: true},
			"negative":                {value: "-1s", wantErr: true},
			"a number without a unit": {value: "60", wantErr: true},
		}
		for name, tt := range tests {
			t.Run(key+"/"+name, func(t *testing.T) {
				clearQUICEnv(t)
				t.Setenv(key, tt.value)

				_, err := quicConfigFromEnv()

				if tt.wantErr {
					assert.ErrorContains(t, err, key)
					return
				}
				assert.NoError(t, err)
			})
		}
	}
}

func TestQUICConfigFromEnv_WindowBounds(t *testing.T) {
	const (
		initialStream = "RELAY_QUIC_INITIAL_STREAM_RECEIVE_WINDOW"
		maxStream     = "RELAY_QUIC_MAX_STREAM_RECEIVE_WINDOW"
		initialConn   = "RELAY_QUIC_INITIAL_CONNECTION_RECEIVE_WINDOW"
		maxConn       = "RELAY_QUIC_MAX_CONNECTION_RECEIVE_WINDOW"
		largestVarint = "4611686018427387903" // 2^62 - 1
		pastVarint    = "4611686018427387904" // quic-go panics on it at the first handshake
	)
	tests := map[string]struct {
		env     map[string]string
		wantErr string // the variable the error names, or "" for none
	}{
		"largest varint as a max window":         {env: map[string]string{maxStream: largestVarint}},
		"initial equal to max":                   {env: map[string]string{initialStream: "1000", maxStream: "1000"}},
		"initial past a varint":                  {env: map[string]string{initialStream: pastVarint, maxStream: largestVarint}, wantErr: initialStream},
		"max past a varint":                      {env: map[string]string{maxStream: pastVarint}, wantErr: maxStream},
		"connection initial past a varint":       {env: map[string]string{initialConn: pastVarint}, wantErr: initialConn},
		"zero window":                            {env: map[string]string{initialStream: "0"}, wantErr: initialStream},
		"initial above max":                      {env: map[string]string{initialStream: "2000", maxStream: "1000"}, wantErr: maxStream},
		"max alone below quic-go's initial":      {env: map[string]string{maxStream: "262144"}, wantErr: maxStream},
		"initial alone above quic-go's max":      {env: map[string]string{initialStream: "8388608"}, wantErr: maxStream},
		"connection max below quic-go's initial": {env: map[string]string{maxConn: "262144"}, wantErr: maxConn},
		"connection initial above its max":       {env: map[string]string{initialConn: "2000", maxConn: "1000"}, wantErr: maxConn},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			clearQUICEnv(t)
			for key, value := range tt.env {
				t.Setenv(key, value)
			}

			_, err := quicConfigFromEnv()

			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			assert.NoError(t, err)
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
		"keep-alive without unit":  {key: "RELAY_QUIC_KEEPALIVE_PERIOD", value: "10"},
		"keep-alive negative":      {key: "RELAY_QUIC_KEEPALIVE_PERIOD", value: "-10s"},
		"handshake timeout words":  {key: "RELAY_QUIC_HANDSHAKE_IDLE_TIMEOUT", value: "soon"},
		"window with a unit":       {key: "RELAY_QUIC_MAX_STREAM_RECEIVE_WINDOW", value: "6MB"},
		"window negative":          {key: "RELAY_QUIC_INITIAL_CONNECTION_RECEIVE_WINDOW", value: "-1"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			clearQUICEnv(t)
			t.Setenv(tt.key, tt.value)

			_, err := quicConfigFromEnv()

			// The message names the variable, so the operator knows which one to fix.
			assert.ErrorContains(t, err, tt.key)
		})
	}
}

func TestQUICEnvSet(t *testing.T) {
	clearQUICEnv(t)
	assert.Empty(t, quicEnvSet(), "nothing set")

	t.Setenv("RELAY_QUIC_MAX_IDLE_TIMEOUT", "2m")
	t.Setenv("RELAY_QUIC_INITIAL_PACKET_SIZE", "1200\nforged=line")

	// In documented order, with line breaks taken out of the value.
	assert.Equal(t, []string{
		"RELAY_QUIC_INITIAL_PACKET_SIZE=1200forged=line",
		"RELAY_QUIC_MAX_IDLE_TIMEOUT=2m",
	}, quicEnvSet())
}
