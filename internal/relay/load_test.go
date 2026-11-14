package relay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLimit_Judge(t *testing.T) {
	tests := map[string]struct {
		max      int64
		wasFull  bool
		value    int64
		wantFull bool
	}{
		"no limit":                    {max: 0, wasFull: true, value: 1 << 40, wantFull: false},
		"below the watermark":         {max: 100, value: 99, wantFull: false},
		"at the watermark":            {max: 100, value: 100, wantFull: true},
		"full stays full above 90%":   {max: 100, wasFull: true, value: 91, wantFull: true},
		"full has room again at 90%":  {max: 100, wasFull: true, value: 90, wantFull: false},
		"room stays room up to below": {max: 100, value: 95, wantFull: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			l := limit{max: tt.max, full: tt.wasFull}

			l.judge(tt.value)

			assert.Equal(t, tt.wantFull, l.full)
		})
	}
}

func TestLimit_Refuses(t *testing.T) {
	tests := map[string]struct {
		max   int64
		value int64
		want  bool
	}{
		"no limit":                            {max: 0, value: 1 << 40, want: false},
		"at the watermark, below the ceiling": {max: 100, value: 109, want: false},
		"at the ceiling":                      {max: 100, value: 110, want: true},
		"a tenth rounds up":                   {max: 15, value: 16, want: false},
		"a tenth rounded up is the ceiling":   {max: 15, value: 17, want: true},
		"a limit of one refuses from two":     {max: 1, value: 2, want: true},
		"a limit of one admits the second":    {max: 1, value: 1, want: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := limit{max: tt.max}.refuses(tt.value)

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRetryAfter(t *testing.T) {
	tests := map[string]struct {
		d    time.Duration
		want string
	}{
		"whole seconds":        {d: 15 * time.Second, want: "15"},
		"a fraction rounds up": {d: 1500 * time.Millisecond, want: "2"},
		"zero":                 {d: 0, want: "0"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, retryAfter(tt.d))
		})
	}
}

func TestLoadMonitor_Tick_EgressRate(t *testing.T) {
	m := newLoadMonitor(0, 0)
	start := time.Unix(1_000_000, 0)

	m.tick(start, 0)
	assert.Zero(t, m.egressKbps.Load(), "one sample has no rate yet")

	// 1,250,000 bytes in 2 s is 5,000 kbit/s.
	m.tick(start.Add(2*time.Second), 1_250_000)
	assert.Equal(t, int64(5000), m.egressKbps.Load())

	// Past the window, the rate spans the window only: the samples before it
	// are dropped but the newest at its start.
	total := int64(1_250_000)
	for i := 2; i <= 10; i++ {
		total += 250_000 // 1,000 kbit/s
		m.tick(start.Add(time.Duration(2*i)*time.Second), total)
	}
	assert.Equal(t, int64(1000), m.egressKbps.Load())
	assert.LessOrEqual(t, len(m.samples), int(egressWindow/loadTick)+1, "samples are bounded by the window")
}

func TestLoadMonitor_Tick_FullAndRefusingOnEgress(t *testing.T) {
	m := newLoadMonitor(0, 1000) // full at 1,000 kbit/s, refusing from 1,100
	at := time.Unix(1_000_000, 0)
	total := int64(0)
	// step sends kbps for one tick and judges.
	step := func(kbps int64) {
		at = at.Add(loadTick)
		total += kbps * 1000 / 8 * int64(loadTick/time.Second)
		m.tick(at, total)
	}
	// admits reports whether a session gets a place, and gives it back.
	admits := func() bool {
		release, ok := m.reserve()
		if ok {
			release()
		}
		return ok
	}
	m.tick(at, 0)

	// The rate is over the window, so a burst shows gradually. Drive it to a
	// steady 1,050 kbit/s: full, still admitting.
	for range 6 {
		step(1050)
	}
	assert.True(t, m.full.Load(), "at the watermark the relay is full")
	assert.True(t, admits(), "below the ceiling it still admits")
	assert.Equal(t, 1.0, testutil.ToFloat64(metricLoadFull))

	for range 6 {
		step(1200)
	}
	assert.False(t, admits(), "at the ceiling it refuses")

	for range 6 {
		step(950)
	}
	assert.True(t, m.full.Load(), "above 90% of the watermark it stays full")
	assert.True(t, admits(), "below the ceiling it admits again")

	for range 6 {
		step(800)
	}
	assert.False(t, m.full.Load(), "at 90% or less it has room again")
	assert.Equal(t, 0.0, testutil.ToFloat64(metricLoadFull))
}

func TestLoadMonitor_Reserve_ClientSessions(t *testing.T) {
	m := newLoadMonitor(10, 0) // full at 10, refusing from 11
	var releases []func()
	for range 11 {
		release, ok := m.reserve()
		require.True(t, ok, "up to the ceiling every session gets a place")
		releases = append(releases, release)
	}

	_, ok := m.reserve()
	assert.False(t, ok, "at the ceiling it refuses, without waiting for a tick")

	releases[0]()
	releases[0]()
	assert.Equal(t, int64(10), m.clients.Load(), "a place is given back once, however often release is called")
	_, ok = m.reserve()
	assert.True(t, ok, "a session ending makes room at once")

	m.tick(time.Unix(1_000_000, 0), 0)
	assert.True(t, m.full.Load(), "eleven sessions are past the watermark: full")
}

func TestLoadMonitor_Reserve_Concurrent(t *testing.T) {
	m := newLoadMonitor(100, 0) // refusing from 110
	var got atomic.Int64
	var wg sync.WaitGroup
	for range 500 {
		wg.Go(func() {
			if _, ok := m.reserve(); ok {
				got.Add(1)
			}
		})
	}
	wg.Wait()

	assert.Equal(t, int64(110), got.Load(), "a burst of connects takes exactly the places under the ceiling")
	assert.Equal(t, int64(110), m.clients.Load())
}

func TestLoadMonitor_Reserve_NilMonitor(t *testing.T) {
	var m *loadMonitor

	release, ok := m.reserve()

	assert.True(t, ok, "a server without a monitor admits")
	release()
}

func TestServer_AdmitClient(t *testing.T) {
	tests := map[string]struct {
		authErr     error
		wantErr     error
		wantClients int64
	}{
		"admitted holds its place":  {wantClients: 1},
		"refused gives it back":     {authErr: auth.RefusedError{Status: http.StatusForbidden}, wantClients: 0},
		"unavailable gives it back": {authErr: errors.New("no key set loaded yet"), wantClients: 0},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fake := &fakeAuth{err: tt.authErr}
			srv := newTestServer("127.0.0.1:0")
			srv.Config = &Config{MaxClientSessions: 10}
			srv.Authorize = fake.authorize
			t.Cleanup(func() { assert.NoError(t, srv.Close()) })
			srv.init()

			_, release, err := srv.admitClient(context.Background(), auth.Request{Event: auth.EventConnect})

			if tt.authErr != nil {
				assert.ErrorIs(t, err, tt.authErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantClients, srv.load.clients.Load())
			release()
			assert.Zero(t, srv.load.clients.Load(), "release gives back what was held")
		})
	}
}

func TestServer_HandleWebTransport_Full(t *testing.T) {
	fake := &fakeAuth{}
	srv := newTestServer("127.0.0.1:0")
	srv.Config = &Config{MaxClientSessions: 10}
	srv.Authorize = fake.authorize
	t.Cleanup(func() { assert.NoError(t, srv.Close()) })
	srv.init()
	for range 11 {
		release, ok := srv.load.reserve()
		require.True(t, ok)
		t.Cleanup(release)
	}
	req := httptest.NewRequest(http.MethodGet, "https://relay.example/acme/app?jwt=a.b.c", nil)
	req.Method = http.MethodConnect
	rec := httptest.NewRecorder()
	refused := metricSessionsRefused.WithLabelValues(refusalFull)

	delta := counterDelta(t, func() { srv.HandleWebTransport(rec, req) }, refused)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "15", rec.Header().Get("Retry-After"), "whole seconds")
	assert.Empty(t, fake.received(), "a full relay refuses before asking")
	assert.Equal(t, 1.0, delta)
	assert.Equal(t, int64(11), srv.load.clients.Load(), "a refused session takes no place")
}
