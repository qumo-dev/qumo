package relay

import (
	"crypto/tls"
	"crypto/x509"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsTrustedPeer(t *testing.T) {
	tests := map[string]struct {
		state *tls.ConnectionState
		want  bool
	}{
		"verified client certificate": {state: &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}, want: true},
		"no client certificate":       {state: &tls.ConnectionState{}, want: false},
		"no TLS state":                {state: nil, want: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := isTrustedPeer(tt.state)

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSplitAddrList(t *testing.T) {
	tests := map[string]struct {
		raw  string
		want []string
	}{
		"empty":                {raw: "", want: nil},
		"one":                  {raw: "hub:4433", want: []string{"hub:4433"}},
		"list with whitespace": {raw: " hub1:4433 , hub2:4433 ,", want: []string{"hub1:4433", "hub2:4433"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := splitAddrList(tt.raw)

			assert.Equal(t, tt.want, got)
		})
	}
}
