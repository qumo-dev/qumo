package envconfig

import (
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestString(t *testing.T) {
	// Use a unique key so a stray value in the ambient environment cannot affect
	// the "unset → default" case.
	key := "QUMO_TEST_ENVCONFIG_STRING"

	os.Unsetenv(key)
	assert.Equal(t, "default", String(key, "default"), "unset → default")

	t.Setenv(key, "actual")
	assert.Equal(t, "actual", String(key, "default"), "set → value")

	t.Setenv(key, "")
	assert.Equal(t, "default", String(key, "default"), "empty → default (empty is treated as unset)")
}

func TestUint(t *testing.T) {
	// A key of this test's own, so the ambient environment cannot set it.
	const key = "QUMO_TEST_ENVCONFIG_UINT"
	tests := map[string]struct {
		value   string
		want    uint64
		wantSet bool
		wantErr bool
		wantIs  error // what the error wraps, when it wraps strconv's
	}{
		"empty is unset":   {value: ""},
		"blank is unset":   {value: " \t"},
		"zero":             {value: "0", wantSet: true},
		"a number":         {value: "1200", want: 1200, wantSet: true},
		"the largest":      {value: "18446744073709551615", want: 1<<64 - 1, wantSet: true},
		"one too large":    {value: "18446744073709551616", wantErr: true, wantIs: strconv.ErrRange},
		"negative":         {value: "-1", wantErr: true},
		"with a unit":      {value: "6MB", wantErr: true, wantIs: strconv.ErrSyntax},
		"with a fraction":  {value: "1.5", wantErr: true},
		"surrounded space": {value: " 12 ", want: 12, wantSet: true},
		"a CRLF ending":    {value: "12\r", want: 12, wantSet: true},
		"space inside":     {value: "1 2", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv(key, tt.value)

			got, set, err := Uint(key)

			if tt.wantErr {
				assert.ErrorContains(t, err, key)
				assert.False(t, set)
				if tt.wantIs != nil {
					assert.ErrorIs(t, err, tt.wantIs)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantSet, set)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDuration(t *testing.T) {
	// A key of this test's own, so the ambient environment cannot set it.
	const key = "QUMO_TEST_ENVCONFIG_DURATION"
	tests := map[string]struct {
		value   string
		want    time.Duration
		wantSet bool
		wantErr bool
	}{
		"empty is unset": {value: ""},
		"blank is unset": {value: "  "},
		"padded":         {value: " 10s\r", want: 10 * time.Second, wantSet: true},
		"zero":           {value: "0", wantSet: true},
		"seconds":        {value: "10s", want: 10 * time.Second, wantSet: true},
		"compound":       {value: "1m30s", want: 90 * time.Second, wantSet: true},
		"no unit":        {value: "10", wantErr: true},
		"negative":       {value: "-1s", wantErr: true},
		"words":          {value: "soon", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv(key, tt.value)

			got, set, err := Duration(key)

			if tt.wantErr {
				assert.ErrorContains(t, err, key)
				assert.False(t, set)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantSet, set)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBool(t *testing.T) {
	// A key of this test's own, so the ambient environment cannot set it.
	const key = "QUMO_TEST_ENVCONFIG_BOOL"
	tests := map[string]struct {
		value string
		def   bool
		want  bool
	}{
		"1":                            {value: "1", want: true},
		"true, in any case":            {value: "TRUE", want: true},
		"yes":                          {value: "yes", want: true},
		"on, with spaces":              {value: " on ", want: true},
		"0 over a true default":        {value: "0", def: true, want: false},
		"false over a true default":    {value: "false", def: true, want: false},
		"no":                           {value: "no", def: true, want: false},
		"off":                          {value: "OFF", def: true, want: false},
		"unset keeps a true default":   {value: "", def: true, want: true},
		"unset keeps a false default":  {value: "", def: false, want: false},
		"anything else is the default": {value: "enabled", def: true, want: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv(key, tt.value)

			got := Bool(key, tt.def)

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSwitch(t *testing.T) {
	// A key of this test's own, so the ambient environment cannot set it.
	const key = "QUMO_TEST_ENVCONFIG_SWITCH"
	tests := map[string]struct {
		value   string
		def     bool
		want    bool
		wantErr bool
	}{
		"true":                        {value: "true", want: true},
		"1":                           {value: "1", want: true},
		"yes in capitals":             {value: "YES", want: true},
		"on with spaces":              {value: " on ", want: true},
		"false over a true default":   {value: "false", def: true, want: false},
		"0 over a true default":       {value: "0", def: true, want: false},
		"no":                          {value: "no", def: true, want: false},
		"off, with a CRLF ending":     {value: "off\r", def: true, want: false},
		"unset keeps a true default":  {value: "", def: true, want: true},
		"unset keeps a false default": {value: "", def: false, want: false},
		"blank keeps the default":     {value: "  ", def: true, want: true},
		"an unknown word":             {value: "maybe", wantErr: true},
		"a single letter":             {value: "t", wantErr: true},
		"a number above 1":            {value: "2", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv(key, tt.value)

			got, err := Switch(key, tt.def)

			if tt.wantErr {
				assert.ErrorContains(t, err, key)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestQUICKeys(t *testing.T) {
	keys := QUICKeys()

	assert.Len(t, keys, 9)
	seen := map[string]bool{}
	for _, key := range keys {
		assert.Regexp(t, `^QUIC_[A-Z_]+$`, key)
		assert.False(t, seen[key], "%s is listed twice", key)
		seen[key] = true
	}
}
