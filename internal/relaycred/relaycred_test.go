package relaycred

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
)

// fakeCredential is a granted credential that records being withdrawn.
type fakeCredential struct {
	maxAge time.Duration
	closed bool
}

func (c *fakeCredential) Grant() moqt.Grant { return moqt.Grant{MaxAge: c.maxAge} }
func (c *fakeCredential) Close() error      { c.closed = true; return nil }

// fakeRelay answers each credential presented: granted for maxAge, or
// refused while refusing is set.
type fakeRelay struct {
	maxAge time.Duration

	mu        sync.Mutex
	refusing  bool
	presented []time.Time
	granted   []*fakeCredential
}

func (r *fakeRelay) refuse(refusing bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refusing = refusing
}

func (r *fakeRelay) present(_ context.Context, _ []byte) (credential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.presented = append(r.presented, time.Now())
	if r.refusing {
		return nil, errors.New("refused")
	}
	c := &fakeCredential{maxAge: r.maxAge}
	r.granted = append(r.granted, c)
	return c, nil
}

func sign() (string, error) { return "token", nil }

// A grant is renewed two thirds through each lifetime, the previous one
// withdrawn once the fresh one is granted, and the last when the session
// ends.
func TestKeep_Renews(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		relay := &fakeRelay{maxAge: 30 * time.Minute}
		ended := false
		start := time.Now()
		done := make(chan struct{})
		go func() {
			defer close(done)
			keep(ctx, relay.present, func() { ended = true }, sign, time.Hour)
		}()

		time.Sleep(time.Hour)
		cancel()
		<-done

		assert.Equal(t, []time.Time{start.Add(40 * time.Minute), start.Add(60 * time.Minute)}, relay.presented,
			"first at 2/3 of the connection's hour, then at 2/3 of each 30-minute grant")
		assert.True(t, relay.granted[0].closed, "replaced by the next")
		assert.True(t, relay.granted[1].closed, "withdrawn with the session")
		assert.False(t, ended)
	})
}

// A credential not granted is presented again, and the session is closed
// once the grant in force lapses without a fresh one.
func TestKeep_ClosesWhenTheGrantLapses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		relay := &fakeRelay{refusing: true}
		var endedAt time.Time
		start := time.Now()

		keep(t.Context(), relay.present, func() { endedAt = time.Now() }, sign, time.Minute)

		assert.Equal(t, start.Add(40*time.Second), relay.presented[0])
		assert.Equal(t, start.Add(50*time.Second), relay.presented[1], "tried again after retryInterval")
		assert.Equal(t, start.Add(50*time.Second), endedAt, "no retry would land before the lapse")
	})
}

// A failure that resolves in time keeps the session.
func TestKeep_RecoversBeforeTheLapse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		relay := &fakeRelay{refusing: true, maxAge: time.Hour}
		ended := false
		done := make(chan struct{})
		go func() {
			defer close(done)
			keep(ctx, relay.present, func() { ended = true }, sign, time.Hour)
		}()

		time.Sleep(40*time.Minute + time.Second)
		relay.refuse(false)
		time.Sleep(retryInterval)
		synctest.Wait()
		cancel()
		<-done

		assert.Len(t, relay.granted, 1)
		assert.False(t, ended)
	})
}
