package relaycred

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/token"
)

// fakeCredential is a granted credential that records being withdrawn.
type fakeCredential struct {
	maxAge time.Duration
	closed bool
}

func (c *fakeCredential) Grant() moqt.Grant { return moqt.Grant{MaxAge: c.maxAge} }
func (c *fakeCredential) Close() error      { c.closed = true; return nil }

// fakeRelay answers each credential presented: granted for maxAge, or
// refused with refusing while it is set.
type fakeRelay struct {
	maxAge time.Duration

	mu        sync.Mutex
	refusing  error
	presented []time.Time
	granted   []*fakeCredential
}

func (r *fakeRelay) refuse(refusing error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refusing = refusing
}

func (r *fakeRelay) present(_ context.Context, _ []byte) (credential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.presented = append(r.presented, time.Now())
	if r.refusing != nil {
		return nil, r.refusing
	}
	c := &fakeCredential{maxAge: r.maxAge}
	r.granted = append(r.granted, c)
	return c, nil
}

func sign() (string, time.Time, error) { return "token", time.Time{}, nil }

var errRefused = &moqt.AuthError{Code: moqt.UnauthorizedSessionErrorCode, Reason: "refused"}

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
			keep(ctx, relay.present, func() { ended = true }, sign, start.Add(time.Hour))
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
// while a retry, which may wait presentTimeout for its answer, would still
// end before the grant in force lapses.
func TestKeep_ClosesBeforeTheGrantLapses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		relay := &fakeRelay{refusing: errRefused}
		var endedAt time.Time
		start := time.Now()

		keep(t.Context(), relay.present, func() { endedAt = time.Now() }, sign, start.Add(2*time.Minute))

		assert.Equal(t, []time.Time{start.Add(80 * time.Second), start.Add(90 * time.Second), start.Add(100 * time.Second)},
			relay.presented, "first at 2/3 of the grant, then every retryInterval")
		assert.Equal(t, start.Add(100*time.Second), endedAt, "20 s left: a retry could outlive the grant")
	})
}

// A relay that verifies no credentials lets the session's grant last: Keep
// stops, and the session stays.
func TestKeep_StopsWhenNotSupported(t *testing.T) {
	for name, refusal := range map[string]error{
		"AUTH_ERROR NOT_SUPPORTED":   &moqt.AuthError{Code: moqt.NotSupportedErrorCode},
		"no Auth Stream on the peer": moqt.ErrAuthNotSupported,
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				relay := &fakeRelay{refusing: refusal}
				ended := false
				start := time.Now()

				keep(t.Context(), relay.present, func() { ended = true }, sign, start.Add(time.Hour))

				assert.Equal(t, []time.Time{start.Add(40 * time.Minute)}, relay.presented)
				assert.False(t, ended)
			})
		})
	}
}

// A failure that resolves in time keeps the session.
func TestKeep_RecoversBeforeTheLapse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		relay := &fakeRelay{refusing: errRefused, maxAge: time.Hour}
		ended := false
		start := time.Now()
		done := make(chan struct{})
		go func() {
			defer close(done)
			keep(ctx, relay.present, func() { ended = true }, sign, start.Add(time.Hour))
		}()

		time.Sleep(40*time.Minute + time.Second)
		relay.refuse(nil)
		time.Sleep(retryInterval)
		synctest.Wait()
		cancel()
		<-done

		assert.Len(t, relay.granted, 1)
		assert.False(t, ended)
	})
}

// Sign's lapse is the token's lifetime from before it was signed.
func TestSign(t *testing.T) {
	key, err := token.GenerateKey("/acme/")
	require.NoError(t, err)
	before := time.Now()

	tok, lapses, err := Sign(key, token.Grant{Root: "/acme/x/", Publish: []string{"/"}})

	require.NoError(t, err)
	assert.NotEmpty(t, tok)
	assert.WithinRange(t, lapses, before.Add(token.MaxLifetime), time.Now().Add(token.MaxLifetime))
	_, _, err = Sign(key, token.Grant{Root: "/other/", Publish: []string{"/"}})
	assert.ErrorIs(t, err, token.ErrForbidden)
}
