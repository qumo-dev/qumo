package relay

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/gomoqt/transport"
	"github.com/qumo-dev/qumo/internal/auth"
)

// Backoff for a revalidate the auth server could not answer: the first retry
// waits about revalidateRetryBase, each later one twice as long, never more
// than revalidateRetryMax. The session lives until its current deadline
// meanwhile, so an outage is bounded by what the server last granted.
const (
	revalidateRetryBase = time.Second
	revalidateRetryMax  = 30 * time.Second
)

// Reasons a session ends, the reason of its end report. A lease ends a
// session for the first three, which are also the reason label of
// metricSessionsEnded and the close message the client sees with
// Unauthorized.
const (
	endExpired = "expired"
	endRefused = "refused"
	endInvalid = "invalid"
	// endClosed is a session the client or the relay closed normally.
	endClosed = "closed"
	// endDropped is a session whose connection was lost: an idle timeout or
	// a stateless reset.
	endDropped = "dropped"
	// endUpgradeFailed is a WebTransport session the auth server admitted
	// but whose upgrade then failed, so it never started.
	endUpgradeFailed = "upgrade_failed"
)

// leasedSession is the part of a session a lease uses: it reads its byte
// totals for a revalidate, and closes it.
type leasedSession interface {
	CloseWithError(code moqt.SessionErrorCode, msg string) error
	Stats() moqt.SessionStats
}

// authorizeFunc asks the auth server about a session: Server.Authorize.
type authorizeFunc func(ctx context.Context, req auth.Request) (*auth.Grant, error)

// lease keeps a checked session within its grant (ADR 0035, #419, #423): it
// ends the session at the grant's expires, and asks the auth server again at
// the grant's revalidate cadence. One timer per session drives both.
//
// A revalidate re-sends the connect request. Its reply decides only whether
// the session continues: a refusal or an unenforceable grant ends it, and an
// admitted grant moves expires and the cadence. The patterns were fixed at
// connect, since the credential is the same, so they are not compared.
type lease struct {
	// ctx is the session's. It ends a revalidate in flight when the session
	// closes.
	ctx       context.Context
	sess      leasedSession
	authorize authorizeFunc
	req       auth.Request

	mu    sync.Mutex
	timer *time.Timer
	// deadline is when the session ends; zero means never.
	deadline time.Time
	// cadence is how often to revalidate; zero means never.
	cadence time.Duration
	// next is when the next revalidate is due, if cadence is set.
	next time.Time
	// failures counts revalidates in a row the auth server could not answer.
	failures int
	stopped  bool
	// ended is why the lease ended the session, or "" while it hasn't.
	ended string
}

// startLease starts the lease of a session admitted by req, which must end at
// deadline (zero: never) and be revalidated every cadence (zero: never). It
// returns nil when there is nothing to do; otherwise the caller stops it when
// the session ends first.
func startLease(ctx context.Context, sess leasedSession, authorize authorizeFunc, req auth.Request, deadline time.Time, cadence time.Duration) *lease {
	if deadline.IsZero() && cadence <= 0 {
		return nil
	}
	l := &lease{
		ctx:       ctx,
		sess:      sess,
		authorize: authorize,
		req:       req,
		deadline:  deadline,
		cadence:   cadence,
	}
	if cadence > 0 {
		l.next = time.Now().Add(cadence)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.timer = time.AfterFunc(l.untilDueLocked(), l.fire)
	return l
}

// stop stops the lease of a session that ended first.
func (l *lease) stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopped = true
	l.timer.Stop()
}

// untilDueLocked returns how long until the next event: the deadline or the
// next revalidate, whichever is first.
func (l *lease) untilDueLocked() time.Duration {
	due := l.deadline
	if l.cadence > 0 && (due.IsZero() || l.next.Before(due)) {
		due = l.next
	}
	return max(time.Until(due), 0)
}

// endedBy returns why the lease ended its session, or "" if it didn't. It is
// safe on a nil lease, which never ends a session.
func (l *lease) endedBy() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ended
}

// fire runs on the timer's goroutine: it ends the session at its deadline,
// or revalidates. It decides under the lock and closes the session after
// releasing it, since a close waits for the session's handlers.
func (l *lease) fire() {
	if reason := l.check(); reason != "" {
		metricSessionsEnded.WithLabelValues(reason).Inc()
		slog.Info("relay: session ended by its grant", "id", l.req.ID, "remote", l.req.Remote, "reason", reason)
		_ = l.sess.CloseWithError(moqt.UnauthorizedSessionErrorCode, reason) // not actionable: the session is ending either way
	}
}

// check runs one due event and returns the reason to end the session, or ""
// to keep it (and rearms the timer when there is more to do).
func (l *lease) check() string {
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		return ""
	}
	if !l.deadline.IsZero() && !time.Now().Before(l.deadline) {
		l.stopped, l.ended = true, endExpired
		l.mu.Unlock()
		return endExpired
	}
	deadline := l.deadline
	l.mu.Unlock()

	g, err := l.revalidate(deadline)

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped {
		return ""
	}
	switch authOutcome(g, err) {
	case authRefused:
		l.stopped, l.ended = true, endRefused
		return endRefused
	case authInvalid:
		slog.Error("relay: the auth server's revalidate grant can't be enforced",
			"id", l.req.ID, "remote", l.req.Remote, "error", err)
		l.stopped, l.ended = true, endInvalid
		return endInvalid
	case authError:
		l.failures++
		slog.Warn("relay: revalidate failed; the session lives until its expires",
			"id", l.req.ID, "remote", l.req.Remote, "attempt", l.failures, "error", err)
		l.next = time.Now().Add(retryDelay(l.failures))
	case authAdmitted:
		l.failures = 0
		// A reply without expires keeps the deadline the session has: a
		// revalidate may move the deadline, never lift it.
		if d := deadlineOf(g); !d.IsZero() {
			l.deadline = d
		}
		l.cadence = g.Revalidate()
		l.next = time.Now().Add(l.cadence)
	case authUnchecked:
		// The auth server stopped checking the session: keep what it has.
		l.failures = 0
		l.next = time.Now().Add(l.cadence)
	}
	if !l.deadline.IsZero() || l.cadence > 0 {
		l.timer.Reset(l.untilDueLocked())
	}
	return ""
}

// revalidate asks the auth server again. The request ends with the session
// or at its deadline, so a stalled auth server can't keep it past expires.
func (l *lease) revalidate(deadline time.Time) (*auth.Grant, error) {
	ctx := l.ctx
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	req := l.req
	req.Event = auth.EventRevalidate
	req.Bytes = sessionBytes(l.sess.Stats())
	g, err := l.authorize(ctx, req)
	metricAuthRequests.WithLabelValues(auth.EventRevalidate, authOutcome(g, err)).Inc()
	return g, err
}

// retryDelay returns the jittered backoff before the attempt-th retry: half
// the exponential delay plus a random part of the other half, so relays that
// lost the auth server together don't retry in step.
func retryDelay(attempt int) time.Duration {
	d := min(revalidateRetryBase<<min(attempt-1, 30), revalidateRetryMax)
	return d/2 + rand.N(d/2+1)
}

// sessionBytes returns a session's cumulative byte totals as the auth server
// receives them.
func sessionBytes(st moqt.SessionStats) auth.Bytes {
	return auth.Bytes{Sent: st.BytesSent, Received: st.BytesReceived}
}

// endReason returns why a checked session ended: the lease's reason when it
// ended the session, otherwise from the session's close cause. A lost
// connection is dropped; any other close is closed.
func endReason(l *lease, cause error) string {
	if reason := l.endedBy(); reason != "" {
		return reason
	}
	if _, ok := errors.AsType[*transport.IdleTimeoutError](cause); ok {
		return endDropped
	}
	if _, ok := errors.AsType[*transport.StatelessResetError](cause); ok {
		return endDropped
	}
	return endClosed
}
