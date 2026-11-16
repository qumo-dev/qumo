package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKeysDownloaded(t *testing.T) {
	tests := map[string]struct {
		value string
		want  bool
	}{
		"an https URL":         {value: "https://cp.example.com/v1/relays/trust", want: true},
		"an http URL":          {value: "http://127.0.0.1:8080/keys", want: true},
		"a scheme in capitals": {value: "HTTPS://cp.example.com/keys", want: true},
		"a file URL":           {value: "file:///etc/qumo/keys.json"},
		"a path":               {value: "/etc/qumo/keys.json"},
		"a Windows path":       {value: `C:\qumo\keys.json`},
		"empty":                {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, KeysDownloaded(tt.value))
		})
	}
}
