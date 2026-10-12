package quicconfig

import (
	"os"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/envconfig"
)

// clearEnv empties every QUIC_* setting for the test, so one left in the
// developer's or the runner's environment cannot reach it.
func clearEnv(tb testing.TB) {
	tb.Helper()
	for _, key := range envconfig.QUICKeys() {
		tb.Setenv(key, "")
	}
}

// testBase is a command's own configuration, as the relay's is: a keep-alive
// period, an idle timeout, and a field no setting touches.
func testBase() *quic.Config {
	return &quic.Config{
		EnableDatagrams:    true,
		KeepAlivePeriod:    10 * time.Second,
		MaxIdleTimeout:     60 * time.Second,
		MaxIncomingStreams: 7,
	}
}

func TestFromEnv_NothingSet(t *testing.T) {
	clearEnv(t)
	base := testBase()

	cfg, err := FromEnv(base)

	require.NoError(t, err)
	assert.Equal(t, base, cfg, "base comes back as it was")
	assert.NotSame(t, base, cfg, "as a copy")
}

func TestFromEnv_NilBase(t *testing.T) {
	clearEnv(t)
	t.Setenv("QUIC_INITIAL_PACKET_SIZE", "1200")

	cfg, err := FromEnv(nil)

	require.NoError(t, err)
	assert.Equal(t, &quic.Config{InitialPacketSize: 1200}, cfg)
}

func TestFromEnv_LeavesBaseAlone(t *testing.T) {
	clearEnv(t)
	t.Setenv("QUIC_IDLE_TIMEOUT", "2m")
	base := testBase()

	_, err := FromEnv(base)

	require.NoError(t, err)
	assert.Equal(t, testBase(), base)
}

func TestFromEnv_BaseKeepAliveFollowsAShorterIdleTimeout(t *testing.T) {
	clearEnv(t)
	t.Setenv("QUIC_IDLE_TIMEOUT", "10s")

	cfg, err := FromEnv(testBase())

	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, cfg.KeepAlivePeriod)
}

func TestFromEnv_KeepAliveAgainstQUICGoIdleTimeout(t *testing.T) {
	// With no idle timeout from base or the environment, quic-go's 30s is
	// what the keep-alive period is held to.
	tests := map[string]struct {
		value   string
		wantErr bool
	}{
		"half of 30s":      {value: "15s"},
		"past half of 30s": {value: "16s", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("QUIC_KEEPALIVE", tt.value)

			_, err := FromEnv(nil)

			if tt.wantErr {
				assert.ErrorContains(t, err, "invalid QUIC_KEEPALIVE ")
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestFromEnv_WindowAgainstBase(t *testing.T) {
	// A window base sets is the one in effect for the other half of its pair.
	tests := map[string]struct {
		base    *quic.Config
		key     string
		value   string
		wantErr bool
	}{
		"max below base's initial":       {base: &quic.Config{InitialStreamReceiveWindow: 2000}, key: "QUIC_MAX_STREAM_RECEIVE_WINDOW", value: "1000", wantErr: true},
		"max at base's initial":          {base: &quic.Config{InitialStreamReceiveWindow: 2000}, key: "QUIC_MAX_STREAM_RECEIVE_WINDOW", value: "2000"},
		"initial above base's max":       {base: &quic.Config{MaxConnectionReceiveWindow: 1000}, key: "QUIC_CONNECTION_RECEIVE_WINDOW", value: "2000", wantErr: true},
		"initial within base's max":      {base: &quic.Config{MaxConnectionReceiveWindow: 4000}, key: "QUIC_CONNECTION_RECEIVE_WINDOW", value: "2000"},
		"base's own pair is not checked": {base: &quic.Config{InitialStreamReceiveWindow: 2000, MaxStreamReceiveWindow: 1000}, key: "QUIC_IDLE_TIMEOUT", value: "1m"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(tt.key, tt.value)

			_, err := FromEnv(tt.base)

			if tt.wantErr {
				assert.ErrorContains(t, err, "invalid "+tt.key+" ")
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestFromEnv_Settings(t *testing.T) {
	clearEnv(t)
	t.Setenv("QUIC_INITIAL_PACKET_SIZE", "1200")
	t.Setenv("QUIC_PMTUD", "off")
	t.Setenv("QUIC_KEEPALIVE", "0")
	t.Setenv("QUIC_IDLE_TIMEOUT", "2m")
	t.Setenv("QUIC_HANDSHAKE_IDLE_TIMEOUT", "3s")
	t.Setenv("QUIC_STREAM_RECEIVE_WINDOW", "1048576")
	t.Setenv("QUIC_MAX_STREAM_RECEIVE_WINDOW", "2097152")
	t.Setenv("QUIC_CONNECTION_RECEIVE_WINDOW", "3145728")
	t.Setenv("QUIC_MAX_CONNECTION_RECEIVE_WINDOW", "4194304")

	cfg, err := FromEnv(testBase())

	require.NoError(t, err)
	assert.Equal(t, uint16(1200), cfg.InitialPacketSize)
	assert.True(t, cfg.DisablePathMTUDiscovery)
	assert.True(t, cfg.EnableDatagrams, "what base fixed stays")
	assert.Equal(t, int64(7), cfg.MaxIncomingStreams, "what no setting touches stays")
	assert.Zero(t, cfg.KeepAlivePeriod)
	assert.Equal(t, 2*time.Minute, cfg.MaxIdleTimeout)
	assert.Equal(t, 3*time.Second, cfg.HandshakeIdleTimeout)
	assert.Equal(t, uint64(1048576), cfg.InitialStreamReceiveWindow)
	assert.Equal(t, uint64(2097152), cfg.MaxStreamReceiveWindow)
	assert.Equal(t, uint64(3145728), cfg.InitialConnectionReceiveWindow)
	assert.Equal(t, uint64(4194304), cfg.MaxConnectionReceiveWindow)
}

func TestFromEnv_InitialPacketSizeBounds(t *testing.T) {
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
			clearEnv(t)
			t.Setenv("QUIC_INITIAL_PACKET_SIZE", tt.value)

			cfg, err := FromEnv(testBase())

			if tt.wantErr {
				assert.ErrorContains(t, err, "QUIC_INITIAL_PACKET_SIZE")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.InitialPacketSize)
		})
	}
}

func TestFromEnv_PMTUDWords(t *testing.T) {
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
			clearEnv(t)
			t.Setenv("QUIC_PMTUD", tt.value)

			cfg, err := FromEnv(testBase())

			if tt.wantErr {
				assert.ErrorContains(t, err, "QUIC_PMTUD")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, !cfg.DisablePathMTUDiscovery)
		})
	}
}

func TestFromEnv_TimeoutBounds(t *testing.T) {
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
				clearEnv(t)
				t.Setenv(key, tt.value)

				_, err := FromEnv(testBase())

				if tt.wantErr {
					assert.ErrorContains(t, err, key)
					return
				}
				assert.NoError(t, err)
			})
		}
	}
}

func TestFromEnv_WindowBounds(t *testing.T) {
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
			clearEnv(t)
			for key, value := range tt.env {
				t.Setenv(key, value)
			}

			_, err := FromEnv(testBase())

			if tt.wantErr != "" {
				// "invalid NAME ..." names a variable the operator set.
				assert.ErrorContains(t, err, "invalid "+tt.wantErr+" ")
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestFromEnv_KeepAlive(t *testing.T) {
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
			clearEnv(t)
			for key, value := range tt.env {
				t.Setenv(key, value)
			}

			cfg, err := FromEnv(testBase())

			if tt.wantErr {
				assert.ErrorContains(t, err, "invalid "+keepAlive+" ")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.KeepAlivePeriod)
		})
	}
}

func TestFromEnv_InvalidValue(t *testing.T) {
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
			clearEnv(t)
			t.Setenv(tt.key, tt.value)

			_, err := FromEnv(testBase())

			// The message names the variable, so the operator knows which one to fix.
			assert.ErrorContains(t, err, tt.key)
		})
	}
}

func TestEnvSet_NothingSet(t *testing.T) {
	clearEnv(t)

	assert.Empty(t, EnvSet())
}

func TestEnvSet(t *testing.T) {
	clearEnv(t)
	t.Setenv("QUIC_IDLE_TIMEOUT", "2m")
	t.Setenv("QUIC_INITIAL_PACKET_SIZE", "1200\nforged=line")

	t.Setenv("QUIC_PMTUD", "  ")

	set := EnvSet()

	// In documented order, with line breaks taken out of the value, and
	// without the blank one, which is read as unset.
	assert.Equal(t, []string{
		"QUIC_INITIAL_PACKET_SIZE=1200forged=line",
		"QUIC_IDLE_TIMEOUT=2m",
	}, set)
}

// quicGoVersion is the quic-go release the defaults and bounds in
// quicconfig.go were read from.
const quicGoVersion = "v0.63.0"

// TestFromEnv_QUICGoVersion fails when go.mod moves to another quic-go, so
// the defaults FromEnv checks against (quicGoIdleTimeout, quicGo*Window)
// and the initial packet size range are read again from the new release before quicGoVersion
// is raised.
func TestFromEnv_QUICGoVersion(t *testing.T) {
	gomod, err := os.ReadFile("../../go.mod")
	require.NoError(t, err)

	assert.Contains(t, string(gomod), "github.com/quic-go/quic-go "+quicGoVersion+"\n")
}
