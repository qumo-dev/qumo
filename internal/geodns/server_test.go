package geodns

import (
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
		"a resolver in Asahikawa gets the north": {
			qname: "cdn.geo.example.com.", qtype: dns.TypeA, from: resolverInAsahikawa,
			expected: []string{"192.0.2.10"},
		},
		"the name is matched without regard to case": {
			qname: "CDN.Geo.Example.com.", qtype: dns.TypeA, from: resolverInAsahikawa,
			expected: []string{"192.0.2.10"},
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

	assert.Equal(t, []string{"192.0.2.10"}, answerAddrs(t, resp), "placed by the resolver")
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
	assert.Equal(t, []string{"192.0.2.10"}, answerAddrs(t, addrs))
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

// serveOnLoopback serves a over UDP and TCP on loopback until the test ends,
// and returns the address of each, by network name.
func serveOnLoopback(tb testing.TB, a *authority) map[string]string {
	tb.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(tb, err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(tb, err)
	for _, srv := range []*dns.Server{{PacketConn: pc, Handler: a}, {Listener: ln, Handler: a}} {
		started := make(chan struct{})
		srv.NotifyStartedFunc = func() { close(started) }
		go func() { _ = srv.ActivateAndServe() }() // stopped by the cleanup below
		<-started
		tb.Cleanup(func() { _ = srv.Shutdown() })
	}
	return map[string]string{"udp": pc.LocalAddr().String(), "tcp": ln.Addr().String()}
}

// TestAuthority_ServeDNS answers over a real UDP socket and over TCP, and
// counts each answer by its response code.
func TestAuthority_ServeDNS(t *testing.T) {
	a := testAuthority(t, testZone())

	for netName, addr := range serveOnLoopback(t, a) {
		t.Run(netName, func(t *testing.T) {
			c := &dns.Client{Net: netName, Timeout: 5 * time.Second}
			resp, _, err := c.Exchange(query("cdn.geo.example.com.", dns.TypeA), addr)
			require.NoError(t, err)
			assert.Equal(t, dns.RcodeSuccess, resp.Rcode)
			// 127.0.0.1 is of unknown place: the regions in turn.
			assert.NotEmpty(t, resp.Answer)

			resp, _, err = c.Exchange(query("www.geo.example.com.", dns.TypeA), addr)
			require.NoError(t, err)
			assert.Equal(t, dns.RcodeNameError, resp.Rcode)
		})
	}
	assert.InDelta(t, 2, testutil.ToFloat64(a.metrics.queries.WithLabelValues("NOERROR")), 0)
	assert.InDelta(t, 2, testutil.ToFloat64(a.metrics.queries.WithLabelValues("NXDOMAIN")), 0)
	assert.InDelta(t, 2, testutil.ToFloat64(a.metrics.located.WithLabelValues("unknown")), 0)
}

// manyAddrs is a zone of one region with n IPv4 addresses.
func manyAddrs(n int) *zone {
	r := region{name: "east", at: tokyo}
	for i := range n {
		r.addrs = append(r.addrs, netip.AddrFrom4([4]byte{192, 0, 2, byte(i + 1)}))
	}
	return &zone{serial: 7, regions: []region{r}}
}

// TestAuthority_ServeDNS_Truncation: a UDP response never exceeds what the
// client takes, nor what this server advertises (so it never fragments);
// TCP carries the whole answer.
func TestAuthority_ServeDNS_Truncation(t *testing.T) {
	const n = 100 // ~1.6 kB of A records: over 512 and over 1232
	a := testAuthority(t, manyAddrs(n))
	a.maxAnswers = n
	addrs := serveOnLoopback(t, a)

	tests := map[string]struct {
		net       string
		edns      uint16 // 0: no EDNS
		truncated bool
		maxSize   int
	}{
		"UDP without EDNS: 512 bytes":       {net: "udp", truncated: true, maxSize: dns.MinMsgSize},
		"UDP with a large buffer: our 1232": {net: "udp", edns: 4096, truncated: true, maxSize: ednsUDPSize},
		"TCP: the whole answer":             {net: "tcp", edns: 4096},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			req := query("cdn.geo.example.com.", dns.TypeA)
			if tt.edns != 0 {
				req.SetEdns0(tt.edns, false)
			}
			c := &dns.Client{Net: tt.net, Timeout: 5 * time.Second, UDPSize: 65535}
			resp, _, err := c.Exchange(req, addrs[tt.net])
			require.NoError(t, err)

			assert.Equal(t, tt.truncated, resp.Truncated)
			if tt.truncated {
				resp.Compress = true // as it was sent
				assert.LessOrEqual(t, resp.Len(), tt.maxSize)
				assert.Less(t, len(resp.Answer), n)
			} else {
				assert.Len(t, resp.Answer, n)
			}
		})
	}
}

func TestUDPLimit(t *testing.T) {
	tests := map[string]struct {
		edns     uint16 // 0: no EDNS
		expected int
	}{
		"no EDNS":                  {expected: dns.MinMsgSize},
		"a buffer below 512":       {edns: 100, expected: dns.MinMsgSize},
		"a buffer between":         {edns: 1000, expected: 1000},
		"a buffer above our limit": {edns: 4096, expected: ednsUDPSize},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			req := query("cdn.geo.example.com.", dns.TypeA)
			if tt.edns != 0 {
				req.SetEdns0(tt.edns, false)
			}
			assert.Equal(t, tt.expected, udpLimit(req))
		})
	}
}

// TestAuthority_Answer_ClientSubnetScope: only the addresses depend on the
// client's subnet; any other answer holds for every client (scope 0), so a
// resolver keeps one copy of it, not one per subnet (RFC 7871 7.2.1).
func TestAuthority_Answer_ClientSubnetScope(t *testing.T) {
	tests := map[string]struct {
		qname string
		qtype uint16
		scope uint8
	}{
		"the served name's addresses": {qname: "cdn.geo.example.com.", qtype: dns.TypeA, scope: 24},
		"no such name":                {qname: "www.geo.example.com.", qtype: dns.TypeA},
		"no data of the type":         {qname: "cdn.geo.example.com.", qtype: dns.TypeMX},
		"the SOA":                     {qname: "geo.example.com.", qtype: dns.TypeSOA},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			req := query(tt.qname, tt.qtype)
			req.SetEdns0(4096, false)
			req.IsEdns0().Option = append(req.IsEdns0().Option, &dns.EDNS0_SUBNET{
				Code: dns.EDNS0SUBNET, Family: 1, SourceNetmask: 24, Address: net.ParseIP("203.0.113.0").To4(),
			})

			resp := testAuthority(t, testZone()).answer(req, resolverInAsahikawa)

			require.NotNil(t, resp.IsEdns0())
			require.Len(t, resp.IsEdns0().Option, 1)
			ecs, ok := resp.IsEdns0().Option[0].(*dns.EDNS0_SUBNET)
			require.True(t, ok)
			assert.Equal(t, uint8(24), ecs.SourceNetmask, "the subnet sent is echoed")
			assert.Equal(t, tt.scope, ecs.SourceScope)
		})
	}
}

func TestAuthority_Answer_ClientSubnetIPv6(t *testing.T) {
	clientInOsakaV6 := netip.MustParseAddr("2001:db8:5::")
	a := testAuthority(t, testZone())
	a.geo = &fakeLocator{at: map[netip.Addr]location{resolverInAsahikawa: asahikawa, clientInOsakaV6: osaka}}
	req := query("cdn.geo.example.com.", dns.TypeA)
	req.SetEdns0(4096, false)
	req.IsEdns0().Option = append(req.IsEdns0().Option, &dns.EDNS0_SUBNET{
		Code: dns.EDNS0SUBNET, Family: 2, SourceNetmask: 56, Address: net.ParseIP("2001:db8:5::"),
	})

	resp := a.answer(req, resolverInAsahikawa)

	assert.Equal(t, []string{"192.0.2.20", "192.0.2.21"}, answerAddrs(t, resp), "the east, for the client in Osaka")
}

// TestAuthority_Answer_NoData covers answers that are "this name, but not
// this type": authoritative, empty, with the SOA for negative caching.
func TestAuthority_Answer_NoData(t *testing.T) {
	v4Only := &zone{serial: 7, regions: []region{
		{name: "east", at: tokyo, addrs: []netip.Addr{netip.MustParseAddr("192.0.2.20")}},
	}}
	tests := map[string]struct {
		name  string // the served name; "" for cdn.geo.example.com.
		zone  *zone
		qname string
		qtype uint16
	}{
		"IPv6 when no region has an IPv6 address": {zone: v4Only, qname: "cdn.geo.example.com.", qtype: dns.TypeAAAA},
		// RFC 8020: NXDOMAIN here would tell a resolver the served name below
		// does not exist either.
		"a name between the apex and the served name": {
			name: "cdn.edge.geo.example.com.", zone: testZone(), qname: "edge.geo.example.com.", qtype: dns.TypeA,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			a := testAuthority(t, tt.zone)
			if tt.name != "" {
				a.name = tt.name
			}

			resp := a.answer(query(tt.qname, tt.qtype), resolverInAsahikawa)

			assert.Equal(t, dns.RcodeSuccess, resp.Rcode)
			assert.True(t, resp.Authoritative)
			assert.Empty(t, resp.Answer)
			require.Len(t, resp.Ns, 1)
			assert.Equal(t, dns.TypeSOA, resp.Ns[0].Header().Rrtype)
		})
	}
}

func TestAuthority_Answer_OtherOpcodes(t *testing.T) {
	for _, opcode := range []int{dns.OpcodeNotify, dns.OpcodeUpdate, dns.OpcodeStatus} {
		req := query("geo.example.com.", dns.TypeSOA)
		req.Opcode = opcode

		resp := testAuthority(t, testZone()).answer(req, resolverInAsahikawa)

		assert.Equal(t, dns.RcodeNotImplemented, resp.Rcode, dns.OpcodeToString[opcode])
		assert.Empty(t, resp.Answer)
	}
}

func TestAuthority_Answer_NoSuchNameBeforeTheFeed(t *testing.T) {
	resp := testAuthority(t, nil).answer(query("www.geo.example.com.", dns.TypeA), resolverInAsahikawa)

	assert.Equal(t, dns.RcodeNameError, resp.Rcode)
	require.Len(t, resp.Ns, 1)
	assert.Equal(t, uint32(1), resp.Ns[0].(*dns.SOA).Serial, "no feed yet: serial 1")
}

// TestAuthority_Answer_WhileTheZoneChanges answers while new feeds are put
// in place; run with -race.
func TestAuthority_Answer_WhileTheZoneChanges(t *testing.T) {
	a := testAuthority(t, testZone())
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 200 {
			if i%2 == 0 {
				a.current.Store(manyAddrs(3))
			} else {
				a.current.Store(testZone())
			}
		}
	})
	for range 4 {
		wg.Go(func() {
			for range 200 {
				resp := a.answer(query("cdn.geo.example.com.", dns.TypeA), clientInOsaka)
				assert.Len(t, resp.Answer, 2)
			}
		})
	}
	wg.Wait()
}

// TestAuthority_Answer_CountsWhereClientsGo: every address query is counted
// by the region answered and whether the client was placed, so how well the
// city database serves a zone shows in the metrics.
func TestAuthority_Answer_CountsWhereClientsGo(t *testing.T) {
	v4Only := &zone{serial: 7, regions: []region{
		{name: "east", at: tokyo, addrs: []netip.Addr{netip.MustParseAddr("192.0.2.20")}},
	}}
	v6Only := &zone{serial: 7, regions: []region{
		{name: "north", at: sapporo, addrs: []netip.Addr{netip.MustParseAddr("2001:db8::10")}},
	}}
	unknownPlace := netip.MustParseAddr("192.0.2.200")
	tests := map[string]struct {
		zone   *zone
		qtype  uint16
		from   netip.Addr
		region string
		client string
	}{
		"a placed client, its nearest region":      {zone: testZone(), qtype: dns.TypeA, from: resolverInAsahikawa, region: "north", client: "located"},
		"a client of unknown place":                {zone: v4Only, qtype: dns.TypeA, from: unknownPlace, region: "east", client: "unknown"},
		"IPv6 from the nearest region that has it": {zone: testZone(), qtype: dns.TypeAAAA, from: clientInOsaka, region: "north", client: "located"},
		"no address of the type: no region":        {zone: v4Only, qtype: dns.TypeAAAA, from: clientInOsaka, region: "", client: "located"},
		"ANY, IPv4 first":                          {zone: v4Only, qtype: dns.TypeANY, from: clientInOsaka, region: "east", client: "located"},
		"ANY with no IPv4, the region of the IPv6": {zone: v6Only, qtype: dns.TypeANY, from: clientInOsaka, region: "north", client: "located"},
		"ANY with no address at all: no region":    {zone: &zone{serial: 7}, qtype: dns.TypeANY, from: clientInOsaka, region: "", client: "located"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			a := testAuthority(t, tt.zone)

			a.answer(query("cdn.geo.example.com.", tt.qtype), tt.from)

			assert.InDelta(t, 1, testutil.ToFloat64(a.metrics.answers.WithLabelValues(tt.region, tt.client)), 0)
			assert.Equal(t, 1, testutil.CollectAndCount(a.metrics.answers), "one series, counted once")
		})
	}
}
