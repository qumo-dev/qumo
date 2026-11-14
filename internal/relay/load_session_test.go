package relay

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests here drive the ceiling through real sessions, end to end.

// A WebSocket session holds its place for as long as it is open: a full
// relay refuses the next handshake, and admits again once one ends.
func TestServer_HandleWebTransport_WebSocketFull(t *testing.T) {
	fake := &fakeAuth{}
	srv := newWebSocketTestServer(t, fake)
	srv.Config.MaxClientSessions = 1 // full at 1, refusing from 2

	first := dialTest(t, srv, "")
	dialTest(t, srv, "")
	require.Eventually(t, func() bool { return srv.load.clients.Load() == 2 }, 5*time.Second, 10*time.Millisecond)

	_, err := dialWebSocket(t, srv, "/acme")

	require.Error(t, err, "the handshake is refused: no session opens")
	assert.Len(t, fake.received(), 2, "the refused session was not checked")

	require.NoError(t, first.CloseWithError(moqt.NoError, "done"))
	require.Eventually(t, func() bool { return srv.load.clients.Load() == 1 }, 5*time.Second, 10*time.Millisecond,
		"a session that ends gives its place back")
	dialTest(t, srv, "")

	// A full relay refuses before the key set is asked, so a third check
	// means the third session was admitted, and it holds a place again.
	assert.Len(t, fake.received(), 3, "the session after one ended was admitted")
	assert.Eventually(t, func() bool { return srv.load.clients.Load() == 2 }, 5*time.Second, 10*time.Millisecond)
}

// run judges on every tick until its context ends.
func TestLoadMonitor_Run(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newLoadMonitor(0, 1000)
		m.egressGauge = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_egress_kbps"})
		var egressBytes int64
		egress := func() int64 {
			egressBytes += 1200 * 1000 / 8 * int64(loadTick/time.Second) // 1,200 kbit/s
			return egressBytes
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			m.run(ctx, egress)
		}()

		time.Sleep(egressWindow + 2*loadTick)
		synctest.Wait()

		assert.Equal(t, int64(1200), m.egressKbps.Load())
		assert.Equal(t, 1200.0, testutil.ToFloat64(m.egressGauge))
		assert.True(t, m.full.Load())
		_, ok := m.reserve()
		assert.False(t, ok, "past the egress ceiling it refuses")

		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			assert.Fail(t, "run did not return when its context ended")
		}
	})
}

// Whether the relay is full on sessions is judged on a tick, with the same
// room at 90% as egress.
func TestLoadMonitor_Tick_SessionsFull(t *testing.T) {
	m := newLoadMonitor(10, 0)
	at := time.Unix(1_000_000, 0)
	var releases []func()
	reserve := func(n int) {
		for range n {
			release, ok := m.reserve()
			require.True(t, ok)
			releases = append(releases, release)
		}
	}
	giveBack := func(n int) {
		for range n {
			releases[len(releases)-1]()
			releases = releases[:len(releases)-1]
		}
	}
	tick := func() bool {
		at = at.Add(loadTick)
		m.tick(at, 0)
		return m.full.Load()
	}

	reserve(9)
	assert.False(t, tick(), "below the watermark")
	reserve(1)
	assert.True(t, tick(), "at the watermark")
	giveBack(1)
	assert.False(t, tick(), "9 of 10 is 90%: room again")
	reserve(1)
	assert.True(t, tick(), "at the watermark again")
	assert.True(t, tick(), "and it stays full while it is")
}
