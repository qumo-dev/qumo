package relay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
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

// resolve returns the addresses (ip:port) the entry names now. An error, or
// an answer with no address, is an error: the caller keeps what it knew.
func (e peerEntry) resolve(ctx context.Context, r peerResolver) ([]string, error) {
	if e.literal() {
		return []string{e.raw}, nil
	}
	if e.port != "" {
		return lookupAddrs(ctx, r, e.host, e.port)
	}

	lookupCtx, cancel := context.WithTimeout(ctx, peerLookupTimeout)
	defer cancel()
	_, records, err := r.LookupSRV(lookupCtx, "", "", e.host)
	if err != nil {
		return nil, fmt.Errorf("look up SRV %s: %w", e.host, err)
	}
	var addrs []string
	for _, rec := range records {
		if rec.Target == "." {
			// RFC 2782: the service is decidedly not available here.
			continue
		}
		// Every target is dialed: a relay peers with all of them, so
		// priority and weight, which choose among them, don't apply.
		got, err := lookupAddrs(ctx, r, rec.Target, fmt.Sprint(rec.Port))
		if err != nil {
			return nil, err
		}
		addrs = append(addrs, got...)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("look up SRV %s: no targets", e.host)
	}
	return addrs, nil
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
	stopped bool // stop was called; touched only by ConnectPeers' goroutine
}

// ConnectPeers dials the configured peer relays, discovering their
// announcements via ANNOUNCE_PLEASE. Received announcements are registered on
// the local TrackMux so that subscribers can transparently access remote
// content.
//
// The names among the peers are resolved again every
// Config.PeerResolveInterval (jittered by ±10%): an address that joins a
// name's answer is dialed, and one that leaves it is dropped, its session
// closed without error. A lookup that fails, or that answers no address,
// keeps the name's last answer, so a DNS outage drops no peer; a name that
// has never resolved is retried sooner, with backoff up to the interval.
// An entry that doesn't parse is logged and skipped.
//
// It blocks until ctx is cancelled and every dial loop has returned.
func (s *Server) ConnectPeers(ctx context.Context) {
	s.init()

	var entries []peerEntry
	for _, p := range s.Config.Peers {
		e, err := parsePeerEntry(p.Address)
		if err != nil {
			slog.Error("relay: peer entry skipped", "error", err)
			continue
		}
		entries = append(entries, e)
	}
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
	dynamic := slices.ContainsFunc(entries, func(e peerEntry) bool { return !e.literal() })

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

	known := make([][]string, len(entries)) // each entry's last answer
	retry := DialBackoff{Base: time.Second, Max: interval}
	for {
		unresolved := false
		want := make(map[string]struct{})
		for i, e := range entries {
			addrs, err := e.resolve(ctx, resolver)
			switch {
			case ctx.Err() != nil:
				return
			case err != nil:
				if known[i] == nil {
					unresolved = true
				}
				slog.Warn("relay: peer lookup failed; keeping its last answer",
					"peer", e.raw, "error", err, "kept", known[i])
			default:
				slices.Sort(addrs)
				addrs = slices.Compact(addrs)
				if !e.literal() && !slices.Equal(addrs, known[i]) {
					slog.Info("relay: peer resolved", "peer", e.raw, "addresses", addrs)
				}
				known[i] = addrs
			}
			for _, a := range known[i] {
				want[a] = struct{}{}
			}
		}

		for addr, run := range runs {
			_, wanted := want[addr]
			switch {
			case !wanted && !run.stopped:
				slog.Info("relay: peer left its name's answer; dropping it", "address", addr)
				run.stop(errPeerLeft)
				run.stopped = true
			case run.stopped && isClosed(run.done):
				delete(runs, addr)
			}
		}
		// A run that returned on its own (the address is this relay) stays
		// in runs, not stopped: it isn't dialed again while it is wanted.
		for addr := range want {
			if run, ok := runs[addr]; !ok || run.stopped {
				start(addr)
			}
		}

		if !dynamic {
			<-ctx.Done()
			return
		}
		if unresolved {
			if !retry.Wait(ctx) {
				return
			}
			continue
		}
		retry.Reset()
		if !sleepCtx(ctx, jitter(interval)) {
			return
		}
	}
}

// jitter returns d scaled by a uniform factor in [0.9, 1.1), so relays
// started together don't query DNS together.
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.9 + 0.2*rand.Float64()))
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
