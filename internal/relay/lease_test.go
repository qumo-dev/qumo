package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/gomoqt/transport"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// leaseRequest is the connect request a lease under test re-sends.
var leaseRequest = auth.Request{ID: "00ff", Event: auth.EventConnect, Path: "/acme", Query: "jwt=h.p.s"}

func TestStartLease_NothingToDo(t *testing.T) {
	assert.Nil(t, startLease(context.Background(), &fakeLeasedSession{}, (&fakeAuth{}).authorize, leaseRequest, 0))
}

// TestLease_RevalidateEnds verifies a refused revalidate ends
// the session at the revalidate, re-sending the connect request as a
// revalidate with the same id.
func TestLease_RevalidateEnds(t *testing.T) {
	tests := map[string]struct {
		err        error
		wantReason string
	}{
		"refused": {err: auth.RefusedError{Status: http.StatusUnauthorized}, wantReason: endRefused},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				server := &fakeAuth{err: tt.err}
				sess := &fakeLeasedSession{}
				sess.setStats(1500, 300)
				l := startLease(t.Context(), sess, server.authorize, leaseRequest, 30*time.Second)
				defer l.stop()

				time.Sleep(30*time.Second - time.Nanosecond)
				synctest.Wait()
				assert.Empty(t, server.received(), "revalidated before the cadence")

				time.Sleep(time.Nanosecond)
				synctest.Wait()
				assert.Equal(t, []sessionClose{{code: moqt.UnauthorizedSessionErrorCode, msg: tt.wantReason}}, sess.closed())
				want := leaseRequest
				want.Event = auth.EventRevalidate
				want.Bytes = auth.Bytes{Sent: 1500, Received: 300}
				assert.Equal(t, []auth.Request{want}, server.received())
			})
		})
	}
}

// TestLease_RevalidateTakesCadence verifies an admitted revalidate keeps the
// session and takes the reply's cadence; its scopes aren't checked.
func TestLease_RevalidateTakesCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The reply's scopes differ from connect's; only the cadence matters.
		server := &fakeAuth{grant: testGrant(t, "other/**", "", time.Minute)}
		sess := &fakeLeasedSession{}
		l := startLease(t.Context(), sess, server.authorize, leaseRequest, 30*time.Second)
		defer l.stop()

		time.Sleep(150 * time.Second)
		synctest.Wait()

		assert.Len(t, server.received(), 3, "revalidates at 30 s, 90 s and 150 s")
		assert.Empty(t, sess.closed())
	})
}

// TestLease_RevalidateUnavailable verifies a revalidate that can't be
// answered leaves the session live, and is retried with backoff.
func TestLease_RevalidateUnavailable(t *testing.T) {
	tests := map[string]*fakeAuth{
		"server errors": {err: errors.New("503")},
		"server stalls": {block: true},
	}
	for name, server := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sess := &fakeLeasedSession{}
				l := startLease(t.Context(), sess, server.authorize, leaseRequest, 30*time.Second)
				defer l.stop()

				time.Sleep(time.Hour)
				synctest.Wait()

				assert.NotEmpty(t, server.received())
				assert.Empty(t, sess.closed(), "a session that couldn't be checked lives on")
			})
		})
	}
	t.Run("retries back off", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			server := &fakeAuth{err: errors.New("503")}
			l := startLease(t.Context(), &fakeLeasedSession{}, server.authorize, leaseRequest, 30*time.Second)
			defer l.stop()

			// Retries follow the first attempt at 30 s with jittered delays of
			// 1, 2, 4, 8, 16, then 30 s; in 3 min at least 6 and at most 13 fit.
			time.Sleep(3 * time.Minute)
			synctest.Wait()
			n := len(server.received())
			assert.GreaterOrEqual(t, n, 6)
			assert.LessOrEqual(t, n, 13)
		})
	})
}

// TestLease_Stop verifies a session that ends first stops its lease: no
// revalidate is sent and the session is not closed.
func TestLease_Stop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server := &fakeAuth{}
		sess := &fakeLeasedSession{}
		l := startLease(t.Context(), sess, server.authorize, leaseRequest, 30*time.Second)

		time.Sleep(10 * time.Second)
		l.stop()
		time.Sleep(2 * time.Minute)
		synctest.Wait()

		assert.Empty(t, server.received())
		assert.Empty(t, sess.closed())
	})
}

func TestRetryDelay(t *testing.T) {
	tests := map[string]struct {
		attempt  int
		min, max time.Duration
	}{
		"first":        {attempt: 1, min: 500 * time.Millisecond, max: time.Second},
		"doubles":      {attempt: 3, min: 2 * time.Second, max: 4 * time.Second},
		"capped":       {attempt: 10, min: 15 * time.Second, max: 30 * time.Second},
		"huge attempt": {attempt: 1000, min: 15 * time.Second, max: 30 * time.Second},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			for range 100 {
				d := retryDelay(tt.attempt)
				require.GreaterOrEqual(t, d, tt.min)
				require.LessOrEqual(t, d, tt.max)
			}
		})
	}
}

// TestLease_RevalidateReportsBytes verifies each revalidate carries the
// session's cumulative byte totals at that moment.
func TestLease_RevalidateReportsBytes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server := &fakeAuth{grant: testGrant(t, "", "**", 30*time.Second)}
		sess := &fakeLeasedSession{}
		l := startLease(t.Context(), sess, server.authorize, leaseRequest, 30*time.Second)
		defer l.stop()

		sess.setStats(100, 10)
		time.Sleep(30 * time.Second)
		synctest.Wait()
		sess.setStats(250, 40)
		time.Sleep(30 * time.Second)
		synctest.Wait()

		var got []auth.Bytes
		for _, req := range server.received() {
			got = append(got, req.Bytes)
		}
		assert.Equal(t, []auth.Bytes{{Sent: 100, Received: 10}, {Sent: 250, Received: 40}}, got)
	})
}

func TestEndReason(t *testing.T) {
	refused := &lease{ended: endRefused}
	tests := map[string]struct {
		lease *lease
		cause error
		want  string
	}{
		"the lease ended it":       {lease: refused, cause: &transport.ApplicationError{}, want: endRefused},
		"closed by an application": {cause: &transport.ApplicationError{}, want: endClosed},
		"idle timeout":             {cause: &transport.IdleTimeoutError{}, want: endDropped},
		"stateless reset":          {cause: &transport.StatelessResetError{}, want: endDropped},
		"wrapped idle timeout":     {cause: fmt.Errorf("session: %w", &transport.IdleTimeoutError{}), want: endDropped},
		"canceled":                 {cause: context.Canceled, want: endClosed},
		"the stream ended without a close": {
			cause: fmt.Errorf("qmux: transport closed: %w", io.EOF), want: endDropped,
		},
		"the stream ended inside a record": {
			cause: fmt.Errorf("qmux: transport closed: %w", io.ErrUnexpectedEOF), want: endDropped,
		},
		"the stream's connection was reset": {
			cause: fmt.Errorf("qmux: transport closed: %w", &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}),
			want:  endDropped,
		},
		"a transport error is a close": {cause: &transport.TransportError{}, want: endClosed},
		"a lease that didn't end it":   {lease: &lease{}, cause: &transport.IdleTimeoutError{}, want: endDropped},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, endReason(tt.lease, tt.cause))
		})
	}
}

// TestLease_RevalidateWithoutCadenceStops verifies an admitted revalidate
// whose grant has no cadence stops revalidating and keeps the session.
func TestLease_RevalidateWithoutCadenceStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server := &fakeAuth{grant: testGrant(t, "", "**", 0)}
		sess := &fakeLeasedSession{}
		l := startLease(t.Context(), sess, server.authorize, leaseRequest, 30*time.Second)
		defer l.stop()

		time.Sleep(time.Hour)
		synctest.Wait()

		assert.Len(t, server.received(), 1, "the reply has no revalidate, so no more are sent")
		assert.Empty(t, sess.closed())
	})
}
