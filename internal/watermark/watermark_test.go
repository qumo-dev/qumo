package watermark

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	tests := map[string]struct {
		value   string
		want    int64
		wantErr bool
	}{
		"unset is no limit": {value: "", want: 0},
		"zero is no limit":  {value: "0", want: 0},
		"a limit":           {value: "80000", want: 80000},
		"negative":          {value: "-1", wantErr: true},
		"not a number":      {value: "80M", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := Parse(EnvMaxEgressKbps, tt.value)

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), EnvMaxEgressKbps)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv(EnvMaxClientSessions, "500")

	got, err := FromEnv(EnvMaxClientSessions)

	require.NoError(t, err)
	assert.Equal(t, int64(500), got)
}

func TestCeiling(t *testing.T) {
	tests := map[string]struct {
		max  int64
		want int64
	}{
		"a tenth above":      {max: 100, want: 110},
		"a tenth rounds up":  {max: 15, want: 17},
		"one refuses at two": {max: 1, want: 2},
		"no limit":           {max: 0, want: 0},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, Ceiling(tt.max))
		})
	}
}

func TestRoom(t *testing.T) {
	tests := map[string]struct {
		max  int64
		want int64
	}{
		"90%":          {max: 100, want: 90},
		"rounded down": {max: 15, want: 13},
		"one has none": {max: 1, want: 0},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, Room(tt.max))
		})
	}
}
