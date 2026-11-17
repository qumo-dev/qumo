package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePeerEntry(t *testing.T) {
	tests := map[string]struct {
		raw         string
		want        peerEntry
		wantLiteral bool
		wantErr     bool
	}{
		"an IPv4 address":                  {raw: "10.0.0.1:4433", want: peerEntry{raw: "10.0.0.1:4433", host: "10.0.0.1", port: "4433"}, wantLiteral: true},
		"an IPv6 address":                  {raw: "[2001:db8::1]:4433", want: peerEntry{raw: "[2001:db8::1]:4433", host: "2001:db8::1", port: "4433"}, wantLiteral: true},
		"a host and port":                  {raw: "hubs.example.com:4433", want: peerEntry{raw: "hubs.example.com:4433", host: "hubs.example.com", port: "4433"}},
		"spaces are trimmed":               {raw: " hubs.example.com:4433 ", want: peerEntry{raw: "hubs.example.com:4433", host: "hubs.example.com", port: "4433"}},
		"an SRV name":                      {raw: "_moqt._udp.hubs.example.com", want: peerEntry{raw: "_moqt._udp.hubs.example.com", host: "_moqt._udp.hubs.example.com"}},
		"a bare service name":              {raw: "role-hub.qumo-relay.service.consul", want: peerEntry{raw: "role-hub.qumo-relay.service.consul", host: "role-hub.qumo-relay.service.consul"}},
		"empty":                            {raw: " ", wantErr: true},
		"an IPv6 address without brackets": {raw: "2001:db8::1", wantErr: true},
		"an IPv6 address without a port":   {raw: "[2001:db8::1]", wantErr: true},
		"no host":                          {raw: ":4433", wantErr: true},
		"an empty port":                    {raw: "hubs.example.com:", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := parsePeerEntry(tt.raw)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantLiteral, got.literal())
		})
	}
}

func TestParsePeerEntries(t *testing.T) {
	entries, err := parsePeerEntries([]Peer{{Address: "10.0.0.1:4433"}, {Address: "_moqt._udp.hubs.example.com"}})
	require.NoError(t, err)
	assert.Len(t, entries, 2)

	_, err = parsePeerEntries([]Peer{{Address: "10.0.0.1:4433"}, {Address: "[2001:db8::1]"}})
	assert.ErrorContains(t, err, "[2001:db8::1]", "the error names the entry")
}

func TestPeerEntry_Resolve(t *testing.T) {
	r := &fakePeerResolver{}
	r.answer("hubs.example.com", "10.0.0.1", "2001:db8::1")
	r.answer("hub-1.example.com", "10.0.0.1")
	r.answer("hub-2.example.com", "10.0.0.2")
	r.answerSRV("_moqt._udp.hubs.example.com",
		&net.SRV{Target: "hub-1.example.com.", Port: 4433, Priority: 1},
		&net.SRV{Target: "hub-2.example.com.", Port: 4434, Priority: 2})
	r.answerSRV("_moqt._udp.unavailable.example.com", &net.SRV{Target: "."})
	r.answerSRV("_moqt._udp.dangling.example.com", &net.SRV{Target: "gone.example.com.", Port: 4433})
	r.answer("hub-1.example.com.", "10.0.0.1")
	r.answer("hub-2.example.com.", "10.0.0.2")

	tests := map[string]struct {
		raw     string
		want    []string
		wantErr bool
	}{
		"a literal address needs no DNS": {raw: "192.0.2.7:4433", want: []string{"192.0.2.7:4433"}},
		"a host is every address it has": {raw: "hubs.example.com:4433", want: []string{"10.0.0.1:4433", "[2001:db8::1]:4433"}},
		// Priority doesn't choose: a relay peers with every target.
		"an SRV name is every target, at its port": {raw: "_moqt._udp.hubs.example.com", want: []string{"10.0.0.1:4433", "10.0.0.2:4434"}},
		"an unknown host":                          {raw: "nowhere.example.com:4433", wantErr: true},
		"an unknown SRV name":                      {raw: "_moqt._udp.nowhere.example.com", wantErr: true},
		"an SRV name of no service":                {raw: "_moqt._udp.unavailable.example.com", wantErr: true},
		"an SRV target that does not resolve":      {raw: "_moqt._udp.dangling.example.com", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			e, err := parsePeerEntry(tt.raw)
			require.NoError(t, err)
			got, err := e.resolve(t.Context(), r)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPeerEntry_Resolve_Failure(t *testing.T) {
	r := &fakePeerResolver{}
	r.answer("hubs.example.com", "10.0.0.1")
	errTimeout := errors.New("i/o timeout")
	r.fail(errTimeout)

	e, err := parsePeerEntry("hubs.example.com:4433")
	require.NoError(t, err)
	_, err = e.resolve(t.Context(), r)
	assert.ErrorIs(t, err, errTimeout)

	literal, err := parsePeerEntry("10.0.0.9:4433")
	require.NoError(t, err)
	got, err := literal.resolve(t.Context(), r)
	require.NoError(t, err, "a literal address asks no DNS")
	assert.Equal(t, []string{"10.0.0.9:4433"}, got)
}

// peerDials counts the dials each peer address got from a test relay whose
// dials never complete: each blocks until its dial loop is stopped, so an
// address that is being dialed stays connected (isConnected) and one that
// is dropped is not.
type peerDials struct {
	mu sync.Mutex
	n  map[string]int
}

func (d *peerDials) count(addr string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n[addr]
}

// newPeerTestServer returns a relay that dials peers through r, resolving
// them again every interval, and the count of its dials.
func newPeerTestServer(tb testing.TB, r peerResolver, interval time.Duration, peers ...string) (*Server, *peerDials) {
	tb.Helper()
	dials := &peerDials{n: make(map[string]int)}
	server := newTestServer("127.0.0.1:0")
	server.MOQDialer = &moqt.Dialer{
		// No dial times out within a test: each lasts until its loop stops.
		Config: &moqt.Config{SetupTimeout: 24 * time.Hour},
		DialQUICFunc: func(ctx context.Context, addr string, _ *tls.Config, _ *quic.Config) (moqt.StreamConn, error) {
			dials.mu.Lock()
			dials.n[addr]++
			dials.mu.Unlock()
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	server.resolver = r
	server.Config = &Config{PeerResolveInterval: interval}
	for _, p := range peers {
		server.Config.Peers = append(server.Config.Peers, Peer{Address: p})
	}
	return server, dials
}

// runPeers runs server.ConnectPeers until the returned stop is called, which
// waits for it to return and stops the server's sampler.
func runPeers(tb testing.TB, server *Server) (stop func()) {
	tb.Helper()
	ctx, cancel := context.WithCancel(tb.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.ConnectPeers(ctx)
	}()
	return func() {
		cancel()
		<-done
		require.NoError(tb, server.Close())
	}
}

// TestServer_ConnectPeers_FollowsAnswer checks that a name's peers follow its
// answer: an address that joins is dialed, one that leaves is dropped, and
// one that stays is left alone.
func TestServer_ConnectPeers_FollowsAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &fakePeerResolver{}
		r.answer("hubs.example.com", "10.0.0.1", "10.0.0.2")
		server, dials := newPeerTestServer(t, r, 30*time.Second, "hubs.example.com:4433", "192.0.2.7:4433")
		stop := runPeers(t, server)
		defer stop()

		synctest.Wait()
		assert.True(t, server.isConnected("10.0.0.1:4433"))
		assert.True(t, server.isConnected("10.0.0.2:4433"))
		assert.True(t, server.isConnected("192.0.2.7:4433"), "a literal address is dialed beside the name")

		r.answer("hubs.example.com", "10.0.0.2", "10.0.0.3")
		time.Sleep(34 * time.Second) // past the interval and its jitter
		synctest.Wait()
		assert.False(t, server.isConnected("10.0.0.1:4433"), "an address that left is dropped")
		assert.True(t, server.isConnected("10.0.0.2:4433"))
		assert.True(t, server.isConnected("10.0.0.3:4433"), "an address that joined is dialed")
		assert.True(t, server.isConnected("192.0.2.7:4433"))
		assert.Equal(t, 1, dials.count("10.0.0.2:4433"), "an address that stayed is not dialed again")

		// Back in the answer, a dropped address is dialed again.
		r.answer("hubs.example.com", "10.0.0.1", "10.0.0.2", "10.0.0.3")
		time.Sleep(34 * time.Second)
		synctest.Wait()
		assert.True(t, server.isConnected("10.0.0.1:4433"))
		assert.Equal(t, 2, dials.count("10.0.0.1:4433"))
	})
}

// TestServer_ConnectPeers_FailureKeepsLastAnswer checks that a lookup that
// fails, or that finds the name gone, drops no peer.
func TestServer_ConnectPeers_FailureKeepsLastAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &fakePeerResolver{}
		r.answer("hubs.example.com", "10.0.0.1", "10.0.0.2")
		server, _ := newPeerTestServer(t, r, 30*time.Second, "hubs.example.com:4433")
		stop := runPeers(t, server)
		defer stop()

		synctest.Wait()
		require.True(t, server.isConnected("10.0.0.1:4433"))

		for _, err := range []error{
			&net.DNSError{Err: "i/o timeout", Name: "hubs.example.com", IsTimeout: true},
			&net.DNSError{Err: "no such host", Name: "hubs.example.com", IsNotFound: true},
		} {
			r.fail(err)
			time.Sleep(34 * time.Second)
			synctest.Wait()
			assert.True(t, server.isConnected("10.0.0.1:4433"), "kept through %v", err)
			assert.True(t, server.isConnected("10.0.0.2:4433"), "kept through %v", err)
		}
	})
}

// TestServer_ConnectPeers_RetriesUnresolvedSooner checks that a name that has
// never resolved is looked up again with backoff, not after a whole interval.
func TestServer_ConnectPeers_RetriesUnresolvedSooner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &fakePeerResolver{}
		server, _ := newPeerTestServer(t, r, 30*time.Second, "_moqt._udp.hubs.example.com")
		stop := runPeers(t, server)
		defer stop()

		synctest.Wait()
		r.answerSRV("_moqt._udp.hubs.example.com", &net.SRV{Target: "hub-1.example.com.", Port: 4433})
		r.answer("hub-1.example.com.", "10.0.0.1")
		time.Sleep(2 * time.Second) // the first retry: 1 s ±25%
		synctest.Wait()
		assert.True(t, server.isConnected("10.0.0.1:4433"))
	})
}

// TestServer_ConnectPeers_SharedAddress checks that an address two entries
// name is dialed once, and kept while either still names it.
func TestServer_ConnectPeers_SharedAddress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &fakePeerResolver{}
		r.answer("hubs.example.com", "10.0.0.1")
		server, dials := newPeerTestServer(t, r, 30*time.Second, "hubs.example.com:4433", "10.0.0.1:4433")
		stop := runPeers(t, server)
		defer stop()

		synctest.Wait()
		assert.Equal(t, 1, dials.count("10.0.0.1:4433"))

		r.answer("hubs.example.com", "10.0.0.2")
		time.Sleep(34 * time.Second)
		synctest.Wait()
		assert.True(t, server.isConnected("10.0.0.1:4433"), "the literal entry still names it")
		assert.Equal(t, 1, dials.count("10.0.0.1:4433"))
	})
}

// TestServer_ConnectPeers_StopsDialLoops checks that ConnectPeers returns
// once ctx ends, with every dial loop stopped.
func TestServer_ConnectPeers_StopsDialLoops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &fakePeerResolver{}
		r.answer("hubs.example.com", "10.0.0.1", "10.0.0.2")
		server, _ := newPeerTestServer(t, r, 30*time.Second, "hubs.example.com:4433")
		stop := runPeers(t, server)

		synctest.Wait()
		stop()
		assert.False(t, server.isConnected("10.0.0.1:4433"))
		assert.False(t, server.isConnected("10.0.0.2:4433"))
	})
}

// TestServer_ConnectPeers_SkipsBadEntry checks that an entry that doesn't
// parse is skipped and the others are dialed.
func TestServer_ConnectPeers_SkipsBadEntry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server, _ := newPeerTestServer(t, &fakePeerResolver{}, 0, "[2001:db8::1]", "10.0.0.1:4433")
		stop := runPeers(t, server)
		defer stop()

		synctest.Wait()
		assert.True(t, server.isConnected("10.0.0.1:4433"))
	})
}

func TestJitter(t *testing.T) {
	for range 100 {
		d := jitter(30 * time.Second)
		assert.GreaterOrEqual(t, d, 27*time.Second)
		assert.Less(t, d, 33*time.Second)
	}
}
