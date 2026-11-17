package relay

import (
	"context"
	"net"
	"sync"
)

var _ peerResolver = (*fakePeerResolver)(nil)

// fakePeerResolver is DNS from tables: hosts maps a name to its addresses,
// srv an SRV name to its records. A name in neither is not found; with err
// set, every lookup fails with it. A zero value knows no name. The relay
// looks up from its own goroutine while a test changes the answers, so
// everything is read and written under mu: tests change it with answer,
// answerSRV and fail.
type fakePeerResolver struct {
	mu    sync.Mutex
	hosts map[string][]net.IPAddr
	srv   map[string][]*net.SRV
	err   error
}

func (f *fakePeerResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	ips, ok := f.hosts[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return ips, nil
}

func (f *fakePeerResolver) LookupSRV(_ context.Context, _, _, name string) (string, []*net.SRV, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", nil, f.err
	}
	records, ok := f.srv[name]
	if !ok {
		return "", nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	return name, records, nil
}

// answer sets host's addresses.
func (f *fakePeerResolver) answer(host string, ips ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hosts == nil {
		f.hosts = make(map[string][]net.IPAddr)
	}
	addrs := make([]net.IPAddr, 0, len(ips))
	for _, ip := range ips {
		addrs = append(addrs, net.IPAddr{IP: net.ParseIP(ip)})
	}
	f.hosts[host] = addrs
}

// answerSRV sets name's SRV records.
func (f *fakePeerResolver) answerSRV(name string, records ...*net.SRV) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.srv == nil {
		f.srv = make(map[string][]*net.SRV)
	}
	f.srv[name] = records
}

// fail makes every lookup fail with err, or, with nil, answer again.
func (f *fakePeerResolver) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}
