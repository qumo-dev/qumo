package relay

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsTrustedPeer(t *testing.T) {
	mesh := []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10")}
	verified := &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}
	unverified := &tls.ConnectionState{}
	udp := func(s string) net.Addr { return net.UDPAddrFromAddrPort(netip.MustParseAddrPort(s)) }

	tests := map[string]struct {
		state  *tls.ConnectionState
		remote net.Addr
		cidrs  []netip.Prefix
		want   bool
	}{
		"verified client cert":                 {state: verified, remote: udp("203.0.113.7:4433"), want: true},
		"verified cert, no cidrs configured":   {state: verified, remote: udp("203.0.113.7:4433"), cidrs: nil, want: true},
		"mesh address":                         {state: unverified, remote: udp("100.101.102.103:51000"), cidrs: mesh, want: true},
		"public address, no cert":              {state: unverified, remote: udp("203.0.113.7:4433"), cidrs: mesh, want: false},
		"no cidrs and no cert":                 {state: unverified, remote: udp("100.101.102.103:51000"), want: false},
		"nil tls state, public address":        {state: nil, remote: udp("198.51.100.1:4433"), cidrs: mesh, want: false},
		"ipv4-mapped mesh address":             {state: unverified, remote: udp("[::ffff:100.64.0.9]:4433"), cidrs: mesh, want: true},
		"just outside the mesh range":          {state: unverified, remote: udp("100.128.0.1:4433"), cidrs: mesh, want: false},
		"nil remote":                           {state: unverified, remote: nil, cidrs: mesh, want: false},
		"ipv6 address in configured ipv6 cidr": {state: unverified, remote: udp("[fd7a:115c:a1e0::1]:4433"), cidrs: []netip.Prefix{netip.MustParsePrefix("fd7a:115c:a1e0::/48")}, want: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, isTrustedPeer(tt.state, tt.remote, tt.cidrs))
		})
	}
}

func TestParsePeerCIDRs(t *testing.T) {
	tests := map[string]struct {
		raw     string
		want    []netip.Prefix
		wantErr bool
	}{
		"empty":                {raw: ""},
		"single":               {raw: "100.64.0.0/10", want: []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10")}},
		"list with whitespace": {raw: " 100.64.0.0/10 , fd7a:115c:a1e0::/48 ,", want: []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("fd7a:115c:a1e0::/48")}},
		"host bits are masked": {raw: "10.1.2.3/8", want: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}},
		"bare address":         {raw: "100.64.0.1", wantErr: true},
		"garbage":              {raw: "mesh", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := parsePeerCIDRs(tt.raw)
			if tt.wantErr {
				assert.ErrorContains(t, err, "PEER_CIDRS")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
