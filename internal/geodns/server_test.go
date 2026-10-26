package geodns

import (
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	resolverInAsahikawa = netip.MustParseAddr("198.51.100.1")
	clientInOsaka       = netip.MustParseAddr("203.0.113.9")
)

// testAuthority answers for geo.example.com with cdn.geo.example.com from z
// (nil: the feed was never read).
func testAuthority(tb testing.TB, z *zone) *authority {
	tb.Helper()
	current := &atomic.Pointer[zone]{}
	if z != nil {
		current.Store(z)
	}
	return &authority{
		zone:        "geo.example.com.",
		name:        "cdn.geo.example.com.",
		nameservers: []string{"ns1.example.com.", "ns2.example.com."},
		hostmaster:  "hostmaster.geo.example.com.",
		ttl:         30,
		maxAnswers:  2,
		current:     current,
		geo: &fakeLocator{at: map[netip.Addr]location{
			resolverInAsahikawa: asahikawa,
			clientInOsaka:       osaka,
		}},
		metrics: newMetrics(prometheus.NewRegistry()),
	}
}

func query(name string, qtype uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	return m
}

func answerAddrs(tb testing.TB, m *dns.Msg) []string {
	tb.Helper()
	var out []string
	for _, rr := range m.Answer {
		switch r := rr.(type) {
		case *dns.A:
			out = append(out, r.A.String())
		case *dns.AAAA:
			out = append(out, r.AAAA.String())
		default:
			tb.Fatalf("unexpected answer %s", rr)
		}
	}
	return out
}

func TestAuthority_Answer_Addresses(t *testing.T) {
	tests := map[string]struct {
		qname    string
		qtype    uint16
		from     netip.Addr
		expected []string
	}{
		"a resolver in Asahikawa gets the north first": {
			qname: "cdn.geo.example.com.", qtype: dns.TypeA, from: resolverInAsahikawa,
			expected: []string{"192.0.2.10", "192.0.2.20"},
		},
		"the name is matched without regard to case": {
			qname: "CDN.Geo.Example.com.", qtype: dns.TypeA, from: resolverInAsahikawa,
			expected: []string{"192.0.2.10", "192.0.2.20"},
		},
		"IPv6": {
			qname: "cdn.geo.example.com.", qtype: dns.TypeAAAA, from: resolverInAsahikawa,
			expected: []string{"2001:db8::10"},
		},
		"ANY answers the IPv4 addresses": {
			qname: "cdn.geo.example.com.", qtype: dns.TypeANY, from: clientInOsaka,
			expected: []string{"192.0.2.20", "192.0.2.21"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			resp := testAuthority(t, testZone()).answer(query(tt.qname, tt.qtype), tt.from)
			assert.Equal(t, dns.RcodeSuccess, resp.Rcode)
			assert.True(t, resp.Authoritative)
			assert.False(t, resp.RecursionAvailable)
			assert.Equal(t, tt.expected, answerAddrs(t, resp))
			for _, rr := range resp.Answer {
				assert.Equal(t, uint32(30), rr.Header().Ttl)
				assert.Equal(t, "cdn.geo.example.com.", rr.Header().Name)
			}
		})
	}
}

func TestAuthority_Answer_ClientSubnet(t *testing.T) {
	// The resolver is in Asahikawa; the client it asks for is in Osaka.
	req := query("cdn.geo.example.com.", dns.TypeA)
	req.SetEdns0(4096, false)
	req.IsEdns0().Option = append(req.IsEdns0().Option, &dns.EDNS0_SUBNET{
		Code: dns.EDNS0SUBNET, Family: 1, SourceNetmask: 24, Address: net.ParseIP("203.0.113.9").To4(),
	})

	resp := testAuthority(t, testZone()).answer(req, resolverInAsahikawa)

	require.Len(t, resp.Answer, 2)
	assert.Equal(t, "192.0.2.20", resp.Answer[0].(*dns.A).A.String(), "the east, for the client in Osaka")
	opt := resp.IsEdns0()
	require.NotNil(t, opt)
	assert.Equal(t, uint16(ednsUDPSize), opt.UDPSize())
	require.Len(t, opt.Option, 1)
	ecs, ok := opt.Option[0].(*dns.EDNS0_SUBNET)
	require.True(t, ok)
	assert.Equal(t, uint8(24), ecs.SourceScope, "the answer holds for the whole subnet sent")
}

func TestAuthority_Answer_ClientSubnetZeroIsIgnored(t *testing.T) {
	req := query("cdn.geo.example.com.", dns.TypeA)
	req.SetEdns0(4096, false)
	req.IsEdns0().Option = append(req.IsEdns0().Option, &dns.EDNS0_SUBNET{
		Code: dns.EDNS0SUBNET, Family: 1, SourceNetmask: 0, Address: net.IPv4zero.To4(),
	})

	resp := testAuthority(t, testZone()).answer(req, resolverInAsahikawa)

	assert.Equal(t, []string{"192.0.2.10", "192.0.2.20"}, answerAddrs(t, resp), "placed by the resolver")
}

func TestAuthority_Answer_Rcodes(t *testing.T) {
	tests := map[string]struct {
		req      *dns.Msg
		zone     *zone
		rcode    int
		auth     bool
		soaInNs  bool
		answerRR uint16
	}{
		"a name outside the zone is refused": {
			req: query("example.org.", dns.TypeA), zone: testZone(), rcode: dns.RcodeRefused,
		},
		"another class is refused": {
			req: func() *dns.Msg {
				m := query("cdn.geo.example.com.", dns.TypeA)
				m.Question[0].Qclass = dns.ClassCHAOS
				return m
			}(),
			zone: testZone(), rcode: dns.RcodeRefused,
		},
		"another name in the zone does not exist": {
			req: query("www.geo.example.com.", dns.TypeA), zone: testZone(), rcode: dns.RcodeNameError, auth: true, soaInNs: true,
		},
		"a name below the served one does not exist": {
			req: query("x.cdn.geo.example.com.", dns.TypeA), zone: testZone(), rcode: dns.RcodeNameError, auth: true, soaInNs: true,
		},
		"another type of the served name has no data": {
			req: query("cdn.geo.example.com.", dns.TypeMX), zone: testZone(), rcode: dns.RcodeSuccess, auth: true, soaInNs: true,
		},
		"the apex has an SOA": {
			req: query("geo.example.com.", dns.TypeSOA), zone: testZone(), rcode: dns.RcodeSuccess, auth: true, answerRR: dns.TypeSOA,
		},
		"the apex has the name servers": {
			req: query("geo.example.com.", dns.TypeNS), zone: testZone(), rcode: dns.RcodeSuccess, auth: true, answerRR: dns.TypeNS,
		},
		"the apex has no address": {
			req: query("geo.example.com.", dns.TypeA), zone: testZone(), rcode: dns.RcodeSuccess, auth: true, soaInNs: true,
		},
		"before the feed is read, the served name fails": {
			req: query("cdn.geo.example.com.", dns.TypeA), rcode: dns.RcodeServerFailure,
		},
		"before the feed is read, the SOA is still answered": {
			req: query("geo.example.com.", dns.TypeSOA), rcode: dns.RcodeSuccess, auth: true, answerRR: dns.TypeSOA,
		},
		"two questions are not implemented": {
			req: func() *dns.Msg {
				m := query("cdn.geo.example.com.", dns.TypeA)
				m.Question = append(m.Question, m.Question[0])
				return m
			}(),
			zone: testZone(), rcode: dns.RcodeNotImplemented,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			resp := testAuthority(t, tt.zone).answer(tt.req, resolverInAsahikawa)
			assert.Equal(t, tt.rcode, resp.Rcode, dns.RcodeToString[resp.Rcode])
			assert.Equal(t, tt.auth, resp.Authoritative)
			if tt.soaInNs {
				require.Len(t, resp.Ns, 1)
				assert.Equal(t, dns.TypeSOA, resp.Ns[0].Header().Rrtype)
				assert.Equal(t, uint32(negativeTTL), resp.Ns[0].Header().Ttl)
			}
			if tt.answerRR != 0 {
				require.NotEmpty(t, resp.Answer)
				for _, rr := range resp.Answer {
					assert.Equal(t, tt.answerRR, rr.Header().Rrtype)
					assert.Equal(t, "geo.example.com.", rr.Header().Name)
				}
			} else if tt.rcode != dns.RcodeSuccess || tt.soaInNs {
				assert.Empty(t, resp.Answer)
			}
		})
	}
}

func TestAuthority_Answer_SOASerial(t *testing.T) {
	resp := testAuthority(t, testZone()).answer(query("geo.example.com.", dns.TypeSOA), resolverInAsahikawa)
	require.Len(t, resp.Answer, 1)
	soa := resp.Answer[0].(*dns.SOA)
	assert.Equal(t, uint32(7), soa.Serial)
	assert.Equal(t, "ns1.example.com.", soa.Ns)
	assert.Equal(t, "hostmaster.geo.example.com.", soa.Mbox)
}

func TestAuthority_Answer_NameAtApex(t *testing.T) {
	a := testAuthority(t, testZone())
	a.name = a.zone

	addrs := a.answer(query("geo.example.com.", dns.TypeA), resolverInAsahikawa)
	assert.Equal(t, []string{"192.0.2.10", "192.0.2.20"}, answerAddrs(t, addrs))
	soa := a.answer(query("geo.example.com.", dns.TypeSOA), resolverInAsahikawa)
	require.Len(t, soa.Answer, 1)
	assert.Equal(t, dns.TypeSOA, soa.Answer[0].Header().Rrtype)
}

func TestAuthority_Answer_RotatesAcrossQueries(t *testing.T) {
	a := testAuthority(t, testZone())
	a.maxAnswers = 1
	first := answerAddrs(t, a.answer(query("cdn.geo.example.com.", dns.TypeA), clientInOsaka))
	second := answerAddrs(t, a.answer(query("cdn.geo.example.com.", dns.TypeA), clientInOsaka))
	assert.ElementsMatch(t, []string{"192.0.2.20", "192.0.2.21"}, append(first, second...))
}

// TestAuthority_ServeDNS answers over a real UDP socket and over TCP.
func TestAuthority_ServeDNS(t *testing.T) {
	a := testAuthority(t, testZone())

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	udp := &dns.Server{PacketConn: pc, Handler: a}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	tcp := &dns.Server{Listener: ln, Handler: a}
	for _, srv := range []*dns.Server{udp, tcp} {
		started := make(chan struct{})
		srv.NotifyStartedFunc = func() { close(started) }
		go func() { _ = srv.ActivateAndServe() }() // stopped by the cleanup below
		<-started
		t.Cleanup(func() { _ = srv.Shutdown() })
	}

	for netName, addr := range map[string]string{"udp": pc.LocalAddr().String(), "tcp": ln.Addr().String()} {
		t.Run(netName, func(t *testing.T) {
			c := &dns.Client{Net: netName, Timeout: 5 * time.Second}
			resp, _, err := c.Exchange(query("cdn.geo.example.com.", dns.TypeA), addr)
			require.NoError(t, err)
			assert.Equal(t, dns.RcodeSuccess, resp.Rcode)
			// 127.0.0.1 is of unknown place: every region in turn, two addresses.
			assert.Len(t, resp.Answer, 2)
		})
	}
}
