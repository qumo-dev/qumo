package geodns

import (
	"net"
	"net/netip"
	"strings"
	"sync/atomic"

	"github.com/miekg/dns"
)

// The SOA timers. The zone changes only through the feed, and secondaries
// are not used: these matter for negative caching (minTTL) and little else.
const (
	soaRefresh = 3600
	soaRetry   = 600
	soaExpire  = 86400
	// negativeTTL is how long a resolver keeps "no such name".
	negativeTTL = 60
	// ednsUDPSize is the UDP payload advertised (the DNS flag day 2020 value).
	ednsUDPSize = 1232
)

// placer places a client's address on the globe; *locator is one.
type placer interface {
	locate(addr netip.Addr) (location, bool)
}

// authority answers for one zone, and the addresses of one name in it.
type authority struct {
	zone        string   // canonical FQDN of the zone apex
	name        string   // canonical FQDN answered with addresses
	nameservers []string // canonical FQDNs, the zone's NS set
	hostmaster  string   // canonical FQDN, the SOA RNAME
	ttl         uint32   // of the address answers
	maxAnswers  int      // addresses in one answer

	current *atomic.Pointer[zone] // nil until the feed is first read
	geo     placer
	turn    atomic.Uint64 // rotates answers across queries
	metrics *metrics
}

var _ dns.Handler = (*authority)(nil)

// ServeDNS answers one query.
func (a *authority) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	resp := a.answer(req, remoteAddr(w.RemoteAddr()))
	if _, udp := w.RemoteAddr().(*net.UDPAddr); udp {
		resp.Truncate(udpLimit(req))
	}
	a.metrics.queries.WithLabelValues(dns.RcodeToString[resp.Rcode]).Inc()
	_ = w.WriteMsg(resp) // not actionable: the client has gone
}

// udpLimit is the largest UDP response to req: 512 bytes without EDNS, else
// what the client takes, up to the size this server advertises, so that a
// response is never large enough to fragment.
func udpLimit(req *dns.Msg) int {
	opt := req.IsEdns0()
	if opt == nil {
		return dns.MinMsgSize
	}
	return min(max(int(opt.UDPSize()), dns.MinMsgSize), ednsUDPSize)
}

// remoteAddr is the address of a query's sender.
func remoteAddr(addr net.Addr) netip.Addr {
	switch a := addr.(type) {
	case *net.UDPAddr:
		ip, _ := netip.AddrFromSlice(a.IP)
		return ip.Unmap()
	case *net.TCPAddr:
		ip, _ := netip.AddrFromSlice(a.IP)
		return ip.Unmap()
	}
	return netip.Addr{}
}

// answer builds the response to req from a sender at from.
func (a *authority) answer(req *dns.Msg, from netip.Addr) *dns.Msg {
	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.RecursionAvailable = false
	if req.Opcode != dns.OpcodeQuery || len(req.Question) != 1 {
		resp.SetRcode(req, dns.RcodeNotImplemented)
		return resp
	}
	q := req.Question[0]
	qname := strings.ToLower(dns.Fqdn(q.Name))
	if q.Qclass != dns.ClassINET || !dns.IsSubDomain(a.zone, qname) {
		// Not ours, and this server does not recurse.
		resp.SetRcode(req, dns.RcodeRefused)
		return resp
	}
	resp.Authoritative = true

	// The client's subnet, when the resolver sends it, places the client
	// better than the resolver's own address does (RFC 7871).
	client := from
	// scope is the subnet option answered: scope 0, an answer for every
	// client, unless the answer is the served name's addresses.
	var scope *dns.EDNS0_SUBNET
	if opt := req.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if s, ok := o.(*dns.EDNS0_SUBNET); ok {
				scope = &dns.EDNS0_SUBNET{
					Code:          dns.EDNS0SUBNET,
					Family:        s.Family,
					SourceNetmask: s.SourceNetmask,
					Address:       s.Address,
				}
				if ip, ok := netip.AddrFromSlice(s.Address); ok && s.SourceNetmask > 0 {
					client = ip.Unmap()
				}
			}
		}
		resp.SetEdns0(ednsUDPSize, false)
		if scope != nil {
			resp.IsEdns0().Option = append(resp.IsEdns0().Option, scope)
		}
	}

	z := a.current.Load()
	switch {
	case qname == a.name && (q.Qtype == dns.TypeA || q.Qtype == dns.TypeAAAA || q.Qtype == dns.TypeANY):
		if z == nil {
			// Never read the feed: no answer is better than a wrong one,
			// and a resolver asks the other servers.
			resp.Authoritative = false
			resp.SetRcode(req, dns.RcodeServerFailure)
			return resp
		}
		resp.Answer = a.addressRecords(z, client, q.Qtype)
		if scope != nil {
			// The addresses hold for the whole of the subnet sent.
			scope.SourceScope = scope.SourceNetmask
		}
	case qname == a.zone:
		switch q.Qtype {
		case dns.TypeSOA:
			resp.Answer = []dns.RR{a.soa(z)}
		case dns.TypeNS, dns.TypeANY:
			resp.Answer = a.nsRecords()
		}
	case qname != a.name && !dns.IsSubDomain(qname, a.name):
		resp.SetRcode(req, dns.RcodeNameError)
	}
	if len(resp.Answer) == 0 {
		// NODATA or NXDOMAIN: the SOA says how long to remember it.
		resp.Ns = []dns.RR{a.soa(z)}
	}
	return resp
}

// addressRecords answers the served name for a client at from.
func (a *authority) addressRecords(z *zone, from netip.Addr, qtype uint16) []dns.RR {
	var at *location
	client := "unknown"
	if loc, ok := a.geo.locate(from); ok {
		at = &loc
		client = "located"
	}
	a.metrics.located.WithLabelValues(client).Inc()
	turn := a.turn.Add(1) - 1
	var out []dns.RR
	answered := ""
	if qtype == dns.TypeA || qtype == dns.TypeANY {
		region, addrs := z.pick(at, false, a.maxAnswers, turn)
		for _, addr := range addrs {
			out = append(out, &dns.A{Hdr: a.header(dns.TypeA, a.ttl), A: addr.AsSlice()})
		}
		answered = region
	}
	if qtype == dns.TypeAAAA || (qtype == dns.TypeANY && len(out) == 0) {
		region, addrs := z.pick(at, true, a.maxAnswers, turn)
		for _, addr := range addrs {
			out = append(out, &dns.AAAA{Hdr: a.header(dns.TypeAAAA, a.ttl), AAAA: addr.AsSlice()})
		}
		answered = region
	}
	if answered == "" {
		answered = "none"
	}
	// Where clients are sent, and whether they were placed to be: how well
	// the city database serves this zone shows here.
	a.metrics.answers.WithLabelValues(answered, client).Inc()
	return out
}

func (a *authority) header(rrtype uint16, ttl uint32) dns.RR_Header {
	name := a.name
	if rrtype == dns.TypeSOA || rrtype == dns.TypeNS {
		name = a.zone
	}
	return dns.RR_Header{Name: name, Rrtype: rrtype, Class: dns.ClassINET, Ttl: ttl}
}

func (a *authority) soa(z *zone) dns.RR {
	serial := uint32(1)
	if z != nil {
		serial = z.serial
	}
	return &dns.SOA{
		Hdr:     a.header(dns.TypeSOA, negativeTTL),
		Ns:      a.nameservers[0],
		Mbox:    a.hostmaster,
		Serial:  serial,
		Refresh: soaRefresh,
		Retry:   soaRetry,
		Expire:  soaExpire,
		Minttl:  negativeTTL,
	}
}

func (a *authority) nsRecords() []dns.RR {
	out := make([]dns.RR, len(a.nameservers))
	for i, ns := range a.nameservers {
		out[i] = &dns.NS{Hdr: a.header(dns.TypeNS, soaRefresh), Ns: ns}
	}
	return out
}
