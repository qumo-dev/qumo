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

// Backoff for a revalidate that could not be answered: the first retry
// waits about revalidateRetryBase, each later one twice as long, never more
// than revalidateRetryMax. The session lives on meanwhile.
const (
	revalidateRetryBase = time.Second
	revalidateRetryMax  = 30 * time.Second
)

// Reasons a session ends, the reason of its end report. A lease ends a
// session for the first, which is also the reason label of
// metricSessionsEnded and the close message the client sees with
// Unauthorized.
const (
	endRefused = "refused"
	// endClosed is a session the client or the relay closed normally.
	endClosed = "closed"
	// endDropped is a session whose connection was lost: an idle timeout or
	// a stateless reset.
	endDropped = "dropped"
	// endUpgradeFailed is a WebTransport session that was admitted but
	// whose upgrade then failed, so it never started.
	endUpgradeFailed = "upgrade_failed"
)

// leasedSession is the part of a session a lease uses: it reads its byte
// totals for a revalidate, and closes it.
type leasedSession interface {
	CloseWithError(code moqt.SessionErrorCode, msg string) error
	Stats() moqt.SessionStats
}

// authorizeFunc checks a session: Server.Authorize.
type authorizeFunc func(ctx context.Context, req auth.Request) (*auth.Grant, error)

// lease checks a checked session again at its grant's revalidate cadence,
// on one timer per session.
//
// A revalidate re-sends the connect request. Its reply decides only whether
// the session continues: a refusal ends it, and an admitted grant sets the
// cadence. The scopes were fixed at connect, since the credential is the
// same, so they are not compared.
type lease struct {
	// ctx is the session's. It ends a revalidate in flight when the session
	// closes.
	ctx       context.Context
	sess      leasedSession
	authorize authorizeFunc
	req       auth.Request

	mu    sync.Mutex
	timer *time.Timer
	// cadence is how often to revalidate.
	cadence time.Duration
	// failures counts revalidates in a row that could not be answered.
	failures int
	stopped  bool
	// ended is why the lease ended the session, or "" while it hasn't.
	ended string
}

// startLease starts the lease of a session admitted by req, to be revalidated
// every cadence. It returns nil for a zero cadence, which never revalidates;
// otherwise the caller stops it when the session ends first.
func startLease(ctx context.Context, sess leasedSession, authorize authorizeFunc, req auth.Request, cadence time.Duration) *lease {
	if cadence <= 0 {
		return nil
	}
	l := &lease{
		ctx:       ctx,
		sess:      sess,
		authorize: authorize,
		req:       req,
		cadence:   cadence,
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.timer = time.AfterFunc(cadence, l.fire)
	return l
}

// stop stops the lease of a session that ended first.
func (l *lease) stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopped = true
	l.timer.Stop()
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

// fire runs on the timer's goroutine: it revalidates, and ends the session if
// that is refused. It decides under the lock and closes the session after
// releasing it, since a close waits for the session's handlers.
func (l *lease) fire() {
	if reason := l.check(); reason != "" {
		metricSessionsEnded.WithLabelValues(reason).Inc()
		slog.Info("relay: session ended by its grant", "id", l.req.ID, "remote", l.req.Remote, "reason", reason)
		_ = l.sess.CloseWithError(moqt.UnauthorizedSessionErrorCode, reason) // not actionable: the session is ending either way
	}
}

// check revalidates once and returns the reason to end the session, or "" to
// keep it (and rearms the timer while there is a cadence).
func (l *lease) check() string {
	l.mu.Lock()
	stopped := l.stopped
	l.mu.Unlock()
	if stopped {
		return ""
	}

	g, err := l.revalidate()

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped {
		return ""
	}
	next := l.cadence
	switch authOutcome(g, err) {
	case authRefused:
		l.stopped, l.ended = true, endRefused
		return endRefused
	case authError:
		l.failures++
		slog.Warn("relay: revalidate failed; the session lives on and is checked again",
			"id", l.req.ID, "remote", l.req.Remote, "attempt", l.failures, "error", err)
		next = retryDelay(l.failures)
	case authAdmitted:
		l.failures = 0
		l.cadence = g.Revalidate()
		next = l.cadence
	case authUnchecked:
		// Authorize stopped checking the session: keep what it has.
		l.failures = 0
	}
	if next > 0 {
		l.timer.Reset(next)
	}
	return ""
}

// revalidate checks the session again. The request ends with the session.
func (l *lease) revalidate() (*auth.Grant, error) {
	req := l.req
	req.Event = auth.EventRevalidate
	req.Bytes = sessionBytes(l.sess.Stats())
	g, err := l.authorize(l.ctx, req)
	metricAuthRequests.WithLabelValues(auth.EventRevalidate, authOutcome(g, err)).Inc()
	return g, err
}

// retryDelay returns the jittered backoff before the attempt-th retry: half
// the exponential delay plus a random part of the other half, so relays that
// failed together don't retry in step.
func retryDelay(attempt int) time.Duration {
	d := min(revalidateRetryBase<<min(attempt-1, 30), revalidateRetryMax)
	return d/2 + rand.N(d/2+1)
}

// sessionBytes returns a session's cumulative byte totals as a Request
// carries them.
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
