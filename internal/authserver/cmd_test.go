package authserver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfig(t *testing.T) {
	tests := map[string]struct {
		keysFile    string
		anonymous   string
		want        config
		wantErrText string
	}{
		"keys only":         {keysFile: "keys.json", want: config{addr: defaultAddr, keysFile: "keys.json"}},
		"anonymous only":    {anonymous: "anon/**, demo/**", want: config{addr: defaultAddr, anonymous: []string{"anon/**", "demo/**"}}},
		"everything":        {anonymous: "**", want: config{addr: defaultAddr, anonymous: []string{"**"}}},
		"neither":           {wantErrText: "neither"},
		"exact path":        {anonymous: "anon", wantErrText: "QUMO_AUTH_ANONYMOUS"},
		"escaping segment":  {anonymous: "anon/../x/**", wantErrText: "QUMO_AUTH_ANONYMOUS"},
		"wildcard segment":  {anonymous: "*/live/**", wantErrText: "QUMO_AUTH_ANONYMOUS"},
		"leading slash":     {anonymous: "/anon/**", wantErrText: "QUMO_AUTH_ANONYMOUS"},
		"empty in the list": {anonymous: "anon/**,", wantErrText: "QUMO_AUTH_ANONYMOUS"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("QUMO_AUTH_ADDR", "")
			t.Setenv("QUMO_AUTH_KEYS_FILE", tt.keysFile)
			t.Setenv("QUMO_AUTH_ANONYMOUS", tt.anonymous)

			got, err := loadConfig()

			if tt.wantErrText != "" {
				assert.ErrorContains(t, err, tt.wantErrText)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
