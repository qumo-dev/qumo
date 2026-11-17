package relay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// defaultPeerResolveInterval is how often the names among the peers are
// resolved again when Config.PeerResolveInterval is unset: the interval at
// which gRPC's DNS resolver and Envoy's DNS clusters refresh by default.
const defaultPeerResolveInterval = 30 * time.Second

// peerLookupTimeout bounds one DNS lookup.
const peerLookupTimeout = 5 * time.Second

// errPeerLeft is why a peer's dial ends when its address leaves the answer
// to the name it came from.
var errPeerLeft = errors.New("peer left its name's answer")

// peerResolver is the DNS the relay finds its peers in: net.DefaultResolver,
// or a fake in tests.
type peerResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
}

var _ peerResolver = (*net.Resolver)(nil)

// peerEntry is one PEERS entry. It takes one of three forms:
//
//   - ip:port, a literal address, dialed as it is;
//   - host:port, a name resolved to all its A/AAAA records, each dialed at
//     port;
//   - a name without a port, looked up as an SRV name as written (such as
//     _moqt._udp.relays.example.com, or a Consul service name), each target
//     resolved to its addresses and dialed at the record's port.
type peerEntry struct {
	raw  string
	host string
	port string // "" for an SRV name
}

// parsePeerEntry reads one PEERS entry.
func parsePeerEntry(raw string) (peerEntry, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return peerEntry{}, errors.New("empty peer entry")
	}
	if !strings.Contains(raw, ":") {
		// No port: an SRV name.
		return peerEntry{raw: raw, host: raw}, nil
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		return peerEntry{}, fmt.Errorf("peer %q: want host:port, ip:port, or an SRV name without a port: %w", raw, err)
	}
	if host == "" || port == "" {
		return peerEntry{}, fmt.Errorf("peer %q: want host:port, ip:port, or an SRV name without a port", raw)
	}
	return peerEntry{raw: raw, host: host, port: port}, nil
}

// parsePeerEntries reads every PEERS entry, failing on the first that
// doesn't parse.
func parsePeerEntries(peers []Peer) ([]peerEntry, error) {
	entries := make([]peerEntry, 0, len(peers))
	for _, p := range peers {
		e, err := parsePeerEntry(p.Address)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// literal reports whether the entry is an address that needs no DNS.
func (e peerEntry) literal() bool {
	return e.port != "" && net.ParseIP(e.host) != nil
}

// peerAnswer is what an entry names: its addresses (ip:port), keyed by
// where they came from. A literal or host:port entry has one key, ""; an
// SRV name has one per target (target:port), so a target whose lookup fails
// can keep its own last addresses.
type peerAnswer map[string][]string

// addrs returns every address in the answer, sorted, without duplicates.
func (a peerAnswer) addrs() []string {
	var all []string
	for _, addrs := range a {
		all = append(all, addrs...)
	}
	slices.Sort(all)
	return slices.Compact(all)
}

// resolve returns what the entry names now; prev is its last answer. A nil
// answer, with an error, means the entry resolved to nothing this time: the
// caller keeps prev. For an SRV name whose query succeeds, a target that
// fails to resolve keeps its addresses from prev, or is left out if it has
// none, and the answer comes with that target's error.
func (e peerEntry) resolve(ctx context.Context, r peerResolver, prev peerAnswer) (peerAnswer, error) {
	if e.literal() {
		return peerAnswer{"": {e.raw}}, nil
	}
	if e.port != "" {
		addrs, err := lookupAddrs(ctx, r, e.host, e.port)
		if err != nil {
			return nil, err
		}
		return peerAnswer{"": addrs}, nil
	}

	lookupCtx, cancel := context.WithTimeout(ctx, peerLookupTimeout)
	defer cancel()
	_, records, err := r.LookupSRV(lookupCtx, "", "", e.host)
	if err != nil {
		return nil, fmt.Errorf("look up SRV %s: %w", e.host, err)
	}
	type target struct {
		key   string
		addrs []string
		err   error
	}
	var targets []*target
	var wg sync.WaitGroup
	for _, rec := range records {
		if rec.Target == "." {
			// RFC 2782: the service is decidedly not available here.
			continue
		}
		// Every target is dialed: a relay peers with all of them, so
		// priority and weight, which choose among them, don't apply.
		port := strconv.Itoa(int(rec.Port))
		t := &target{key: net.JoinHostPort(rec.Target, port)}
		targets = append(targets, t)
		wg.Go(func() { t.addrs, t.err = lookupAddrs(ctx, r, rec.Target, port) })
	}
	wg.Wait()

	answer := make(peerAnswer, len(targets))
	var errs []error
	for _, t := range targets {
		switch {
		case t.err == nil:
			answer[t.key] = t.addrs
		case prev[t.key] != nil:
			answer[t.key] = prev[t.key]
			errs = append(errs, fmt.Errorf("%w (keeping its last addresses)", t.err))
		default:
			errs = append(errs, t.err)
		}
	}
	if len(answer) == 0 {
		errs = append(errs, errors.New("no target resolved"))
		return nil, fmt.Errorf("look up SRV %s: %w", e.host, errors.Join(errs...))
	}
	if len(errs) > 0 {
		return answer, fmt.Errorf("look up SRV %s: %w", e.host, errors.Join(errs...))
	}
	return answer, nil
}

// lookupAddrs resolves host to ip:port for each of its addresses.
func lookupAddrs(ctx context.Context, r peerResolver, host, port string) ([]string, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, peerLookupTimeout)
	defer cancel()
	ips, err := r.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return nil, fmt.Errorf("look up %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("look up %s: no addresses", host)
	}
	addrs := make([]string, 0, len(ips))
	for _, ip := range ips {
		addrs = append(addrs, net.JoinHostPort(ip.IP.String(), port))
	}
	return addrs, nil
}

// peerRun is the dial loop (maintainPeer) of one peer address.
type peerRun struct {
	stop    context.CancelCauseFunc
	done    chan struct{}
	stopped bool // stop was called; touched only by connectPeers' goroutine
}

// ConnectPeers dials the relays in Config.Peers, as connectPeers does. It
// returns an error, before dialing anything, if an entry doesn't parse, and
// nil once ctx ends.
func (s *Server) ConnectPeers(ctx context.Context) error {
	entries, err := parsePeerEntries(s.Config.Peers)
	if err != nil {
		return err
	}
	s.connectPeers(ctx, entries)
	return nil
}

// connectPeers dials the peer relays entries name, discovering their
// announcements via ANNOUNCE_PLEASE. Received announcements are registered
// on the local TrackMux so that subscribers can transparently access remote
// content.
//
// Literal addresses are dialed at once. The names are looked up together,
// and again every Config.PeerResolveInterval: an address that joins a name's
// answer is dialed, and one that leaves it is dropped, its session closed
// without error. A lookup that fails, or that answers no address, keeps the
// name's last answer, so a DNS outage drops no peer; a name that has never
// resolved is retried sooner, with backoff up to the interval.
//
// It blocks until ctx is cancelled and every dial loop has returned.
func (s *Server) connectPeers(ctx context.Context, entries []peerEntry) {
	s.init()
	if len(entries) == 0 {
		return
	}
	resolver := s.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	interval := s.Config.PeerResolveInterval
	if interval <= 0 {
		interval = defaultPeerResolveInterval
	}

	var wg sync.WaitGroup
	defer wg.Wait()

	runs := make(map[string]*peerRun)
	start := func(addr string) {
		prev := runs[addr] // a stopped run of the same address, maybe still unwinding
		pctx, stop := context.WithCancelCause(ctx)
		run := &peerRun{stop: stop, done: make(chan struct{})}
		runs[addr] = run
		wg.Go(func() {
			defer close(run.done)
			if prev != nil {
				// Its session must be gone before this one is recorded.
				select {
				case <-prev.done:
				case <-pctx.Done():
					return
				}
			}
			if !s.markConnected(addr) {
				return
			}
			defer s.markUnconnected(addr)
			s.maintainPeer(pctx, Peer{Address: addr})
		})
	}
	// dial makes the running dial loops those of the addresses in want.
	dial := func(want map[string]struct{}) {
		for addr, run := range runs {
			_, wanted := want[addr]
			switch {
			case !wanted && !run.stopped:
				slog.Info("relay: peer left its name's answer; dropping it", "address", addr)
				run.stop(errPeerLeft)
				run.stopped = true
			case run.stopped && isClosed(run.done):
				// Its loop is over and nothing dials the address: its
				// series go too, so they don't pile up as addresses change.
				delete(runs, addr)
				forgetPeerMetrics(addr)
			}
		}
		// A run that returned on its own (the address is this relay) stays
		// in runs, not stopped: it isn't dialed again while it is wanted.
		for addr := range want {
			if run, ok := runs[addr]; !ok || run.stopped {
				start(addr)
			}
		}
	}

	// Literal addresses need no DNS: they are dialed before any name is
	// looked up.
	literals := make(map[string]struct{})
	for _, e := range entries {
		if e.literal() {
			literals[e.raw] = struct{}{}
		}
	}
	dial(literals)
	if len(literals) == len(entries) {
		<-ctx.Done()
		return
	}

	known := make([]peerAnswer, len(entries)) // each entry's last answer
	retry := DialBackoff{Base: time.Second, Max: interval}
	for {
		answers := make([]peerAnswer, len(entries))
		errs := make([]error, len(entries))
		var lookups sync.WaitGroup
		for i, e := range entries {
			lookups.Go(func() { answers[i], errs[i] = e.resolve(ctx, resolver, known[i]) })
		}
		lookups.Wait()
		if ctx.Err() != nil {
			return
		}

		unresolved := false
		want := make(map[string]struct{})
		for i, e := range entries {
			switch {
			case answers[i] == nil:
				if known[i] == nil {
					unresolved = true
				}
				slog.Warn("relay: peer lookup failed; keeping its last answer",
					"peer", e.raw, "error", errs[i], "kept", known[i].addrs())
			case errs[i] != nil:
				slog.Warn("relay: peer lookup partly failed", "peer", e.raw, "error", errs[i])
			}
			if answers[i] != nil {
				if !e.literal() && !slices.Equal(answers[i].addrs(), known[i].addrs()) {
					slog.Info("relay: peer resolved", "peer", e.raw, "addresses", answers[i].addrs())
				}
				known[i] = answers[i]
			}
			for _, a := range known[i].addrs() {
				want[a] = struct{}{}
			}
		}
		dial(want)

		if unresolved {
			if !retry.Wait(ctx) {
				return
			}
			continue
		}
		retry.Reset()
		if !sleepCtx(ctx, interval) {
			return
		}
	}
}

// forgetPeerMetrics deletes the series labelled with a peer's address.
func forgetPeerMetrics(addr string) {
	metricPeerDialAttempts.DeletePartialMatch(prometheus.Labels{"peer": addr})
	metricDialRetriesTotal.DeleteLabelValues(addr)
}

// sleepCtx waits d, and reports false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// isClosed reports whether ch is closed.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
