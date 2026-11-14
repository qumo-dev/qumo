package watermark

import (
	"math"
	"strconv"
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
		"unset is no limit":         {value: "", want: 0},
		"zero is no limit":          {value: "0", want: 0},
		"a limit":                   {value: "80000", want: 80000},
		"the largest limit":         {value: strconv.FormatInt(math.MaxInt64, 10), want: math.MaxInt64},
		"negative":                  {value: "-1", wantErr: true},
		"not a number":              {value: "80M", wantErr: true},
		"a fraction":                {value: "1.5", wantErr: true},
		"surrounding spaces":        {value: " 80000 ", wantErr: true},
		"past the largest":          {value: "9223372036854775808", wantErr: true},
		"a sign on a positive":      {value: "+80000", want: 80000},
		"leading zeros are base 10": {value: "0100", want: 100},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := Parse(EnvMaxEgressKbps, tt.value)

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), EnvMaxEgressKbps)
				assert.Contains(t, err.Error(), strconv.Quote(tt.value), "the error names the value given")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFromEnv(t *testing.T) {
	tests := map[string]struct {
		set     bool
		value   string
		want    int64
		wantErr bool
	}{
		"set":     {set: true, value: "500", want: 500},
		"unset":   {set: false, want: 0},
		"invalid": {set: true, value: "lots", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv(EnvMaxClientSessions, "")
			if tt.set {
				t.Setenv(EnvMaxClientSessions, tt.value)
			}

			got, err := FromEnv(EnvMaxClientSessions)

			if tt.wantErr {
				assert.ErrorContains(t, err, EnvMaxClientSessions)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCeiling(t *testing.T) {
	tests := map[string]struct {
		max  int64
		want int64
	}{
		"a tenth above":                  {max: 100, want: 110},
		"a tenth rounds up":              {max: 15, want: 17},
		"one refuses at two":             {max: 1, want: 2},
		"nine refuses at ten":            {max: 9, want: 10},
		"no limit":                       {max: 0, want: 0},
		"the largest saturates":          {max: math.MaxInt64, want: math.MaxInt64},
		"just short of overflow fits":    {max: math.MaxInt64 / 11 * 10, want: math.MaxInt64 / 11 * 11},
		"close to the largest saturates": {max: math.MaxInt64 - 100, want: math.MaxInt64},
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
		"90%":                      {max: 100, want: 90},
		"rounded down":             {max: 15, want: 13},
		"one has none":             {max: 1, want: 0},
		"no limit":                 {max: 0, want: 0},
		"the largest, no overflow": {max: math.MaxInt64, want: math.MaxInt64 - math.MaxInt64/10 - 1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, Room(tt.max))
		})
	}
}

// The thresholds keep their order for every limit: room again below full,
// full below refusing, so that a relay is offered last before it refuses
// and never flaps between full and not.
func TestThresholds_Ordered(t *testing.T) {
	limits := []int64{1, 2, 9, 10, 11, 15, 99, 100, 101, 1_000, 80_000, 75_000, math.MaxInt64 / 2, math.MaxInt64 - 1, math.MaxInt64}
	for _, max := range limits {
		assert.Less(t, Room(max), max, "room below the watermark for %d", max)
		assert.GreaterOrEqual(t, Room(max), int64(0), "room is never negative for %d", max)
		if max < math.MaxInt64 {
			assert.Greater(t, Ceiling(max), max, "the ceiling above the watermark for %d", max)
		}
	}
}
