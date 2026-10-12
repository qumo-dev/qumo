package relay

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearQUICEnv empties every QUIC_* setting for the test, so one left in
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
	t.Setenv("QUIC_INITIAL_PACKET_SIZE", "1200")
	t.Setenv("QUIC_PMTUD", "off")
	t.Setenv("QUIC_KEEPALIVE", "0")
	t.Setenv("QUIC_IDLE_TIMEOUT", "2m")
	t.Setenv("QUIC_HANDSHAKE_IDLE_TIMEOUT", "3s")
	t.Setenv("QUIC_STREAM_RECEIVE_WINDOW", "1048576")
	t.Setenv("QUIC_MAX_STREAM_RECEIVE_WINDOW", "2097152")
	t.Setenv("QUIC_CONNECTION_RECEIVE_WINDOW", "3145728")
	t.Setenv("QUIC_MAX_CONNECTION_RECEIVE_WINDOW", "4194304")

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
			t.Setenv("QUIC_INITIAL_PACKET_SIZE", tt.value)

			cfg, err := quicConfigFromEnv()

			if tt.wantErr {
				assert.ErrorContains(t, err, "QUIC_INITIAL_PACKET_SIZE")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.InitialPacketSize)
		})
	}
}

func TestQUICConfigFromEnv_PMTUDWords(t *testing.T) {
	tests := map[string]struct {
		value   string
		want    bool // whether path MTU discovery stays on
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
		"only spaces":      {value: "  ", want: true},
		"not a switch":     {value: "maybe", wantErr: true},
		"a single letter":  {value: "t", wantErr: true},
		"a number above 1": {value: "2", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			clearQUICEnv(t)
			t.Setenv("QUIC_PMTUD", tt.value)

			cfg, err := quicConfigFromEnv()

			if tt.wantErr {
				assert.ErrorContains(t, err, "QUIC_PMTUD")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, !cfg.DisablePathMTUDiscovery)
		})
	}
}

func TestQUICConfigFromEnv_TimeoutBounds(t *testing.T) {
	// The two timeouts refuse zero, which quic-go would read as "use my
	// default", and anything a peer would be told is no timeout at all.
	for _, key := range []string{"QUIC_IDLE_TIMEOUT", "QUIC_HANDSHAKE_IDLE_TIMEOUT"} {
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
		initialStream = "QUIC_STREAM_RECEIVE_WINDOW"
		maxStream     = "QUIC_MAX_STREAM_RECEIVE_WINDOW"
		initialConn   = "QUIC_CONNECTION_RECEIVE_WINDOW"
		maxConn       = "QUIC_MAX_CONNECTION_RECEIVE_WINDOW"
		largestVarint = "4611686018427387903" // 2^62 - 1
		pastVarint    = "4611686018427387904" // quic-go panics on it at the first handshake
	)
	tests := map[string]struct {
		env     map[string]string
		wantErr string // the variable the error calls invalid, or "" for none
	}{
		"largest varint as a max window":         {env: map[string]string{maxStream: largestVarint}},
		"initial equal to max":                   {env: map[string]string{initialStream: "1000", maxStream: "1000"}},
		"initial past a varint":                  {env: map[string]string{initialStream: pastVarint, maxStream: largestVarint}, wantErr: initialStream},
		"max past a varint":                      {env: map[string]string{maxStream: pastVarint}, wantErr: maxStream},
		"connection initial past a varint":       {env: map[string]string{initialConn: pastVarint}, wantErr: initialConn},
		"zero window":                            {env: map[string]string{initialStream: "0"}, wantErr: initialStream},
		"initial above max":                      {env: map[string]string{initialStream: "2000", maxStream: "1000"}, wantErr: maxStream},
		"max alone below quic-go's initial":      {env: map[string]string{maxStream: "262144"}, wantErr: maxStream},
		"initial alone above quic-go's max":      {env: map[string]string{initialStream: "8388608"}, wantErr: initialStream},
		"connection initial above quic-go's max": {env: map[string]string{initialConn: "16777216"}, wantErr: initialConn},
		"initial alone at quic-go's max":         {env: map[string]string{initialStream: "6291456"}},
		"max alone at quic-go's initial":         {env: map[string]string{maxStream: "524288"}},
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
				// "invalid NAME ..." names a variable the operator set.
				assert.ErrorContains(t, err, "invalid "+tt.wantErr+" ")
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestQUICConfigFromEnv_KeepAlive(t *testing.T) {
	const (
		keepAlive = "QUIC_KEEPALIVE"
		idle      = "QUIC_IDLE_TIMEOUT"
	)
	tests := map[string]struct {
		env     map[string]string
		want    time.Duration
		wantErr bool
	}{
		"unset keeps the default":               {want: 10 * time.Second},
		"unset follows a shorter idle timeout":  {env: map[string]string{idle: "10s"}, want: 5 * time.Second},
		"unset ignores a longer idle timeout":   {env: map[string]string{idle: "10m"}, want: 10 * time.Second},
		"zero sends none":                       {env: map[string]string{keepAlive: "0"}, want: 0},
		"exactly half the default idle timeout": {env: map[string]string{keepAlive: "30s"}, want: 30 * time.Second},
		"past half the default idle timeout":    {env: map[string]string{keepAlive: "31s"}, wantErr: true},
		"past half a set idle timeout":          {env: map[string]string{keepAlive: "10s", idle: "15s"}, wantErr: true},
		"half a set idle timeout":               {env: map[string]string{keepAlive: "10s", idle: "20s"}, want: 10 * time.Second},
		"one millisecond":                       {env: map[string]string{keepAlive: "1ms"}, want: time.Millisecond},
		"under a millisecond":                   {env: map[string]string{keepAlive: "1ns"}, wantErr: true},
		"padded with space, as an env file may": {env: map[string]string{keepAlive: " 5s\r"}, want: 5 * time.Second},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			clearQUICEnv(t)
			for key, value := range tt.env {
				t.Setenv(key, value)
			}

			cfg, err := quicConfigFromEnv()

			if tt.wantErr {
				assert.ErrorContains(t, err, "invalid "+keepAlive+" ")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.KeepAlivePeriod)
		})
	}
}

func TestQUICConfigFromEnv_InvalidValue(t *testing.T) {
	tests := map[string]struct {
		key   string
		value string
	}{
		"packet size not a number": {key: "QUIC_INITIAL_PACKET_SIZE", value: "big"},
		"packet size negative":     {key: "QUIC_INITIAL_PACKET_SIZE", value: "-1200"},
		"keep-alive without unit":  {key: "QUIC_KEEPALIVE", value: "10"},
		"keep-alive negative":      {key: "QUIC_KEEPALIVE", value: "-10s"},
		"handshake timeout words":  {key: "QUIC_HANDSHAKE_IDLE_TIMEOUT", value: "soon"},
		"window with a unit":       {key: "QUIC_MAX_STREAM_RECEIVE_WINDOW", value: "6MB"},
		"window negative":          {key: "QUIC_CONNECTION_RECEIVE_WINDOW", value: "-1"},
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

func TestQUICEnvSet_NothingSet(t *testing.T) {
	clearQUICEnv(t)

	assert.Empty(t, quicEnvSet())
}

func TestQUICEnvSet(t *testing.T) {
	clearQUICEnv(t)
	t.Setenv("QUIC_IDLE_TIMEOUT", "2m")
	t.Setenv("QUIC_INITIAL_PACKET_SIZE", "1200\nforged=line")

	t.Setenv("QUIC_PMTUD", "  ")

	set := quicEnvSet()

	// In documented order, with line breaks taken out of the value, and
	// without the blank one, which is read as unset.
	assert.Equal(t, []string{
		"QUIC_INITIAL_PACKET_SIZE=1200forged=line",
		"QUIC_IDLE_TIMEOUT=2m",
	}, set)
}

// quicGoVersion is the quic-go release the defaults and bounds in
// quic_config.go were read from.
const quicGoVersion = "v0.63.0"

// TestQUICWindows_QUICGoVersion fails when go.mod moves to another quic-go, so
// the defaults quicWindows checks against (quicGo*Window) and the initial
// packet size range are read again from the new release before quicGoVersion
// is raised.
func TestQUICWindows_QUICGoVersion(t *testing.T) {
	gomod, err := os.ReadFile("../../go.mod")
	require.NoError(t, err)

	assert.Contains(t, string(gomod), "github.com/quic-go/quic-go "+quicGoVersion+"\n")
}
