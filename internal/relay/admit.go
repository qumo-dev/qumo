package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
)

// admission is a session's grant, carried in the session's context so that
// both checks find it: announcements (serveSession) and subscriptions
// (authorizeSubscribe), whose TrackWriter context derives from the session's.
// A WebTransport session's is decided at the upgrade. A native-QUIC session's
// is pending from ConnContext until relayPeer decides it, and a subscription
// arriving before then waits.
type admission struct {
	decided chan struct{}
	// grant is set before decided closes. nil is unchecked: a trusted peer.
	grant *auth.Grant
	// deadline is when the session must end, set with grant: the grant's
	// expires, taken on the monotonic clock when the grant is accepted so
	// that a wall-clock jump doesn't move it. Zero means never.
	deadline time.Time
}

type admissionKey struct{}

// refusedGrant is the grant of a refused session: it covers nothing.
var refusedGrant = &auth.Grant{}

func pendingAdmission() *admission {
	return &admission{decided: make(chan struct{})}
}

func decidedAdmission(g *auth.Grant) *admission {
	a := pendingAdmission()
	a.decide(g)
	return a
}

func (a *admission) decide(g *auth.Grant) {
	a.grant = g
	if g != nil && !g.Expires().IsZero() {
		// time.Now carries a monotonic reading and Add keeps it, so the
		// deadline is measured from now on the monotonic clock.
		a.deadline = time.Now().Add(time.Until(g.Expires()))
	}
	close(a.decided)
}

func withAdmission(ctx context.Context, a *admission) context.Context {
	return context.WithValue(ctx, admissionKey{}, a)
}

func admissionFrom(ctx context.Context) *admission {
	a, _ := ctx.Value(admissionKey{}).(*admission)
	return a
}

// sessionGrant returns the grant of the session ctx belongs to, waiting for a
// pending admission. nil is unchecked; so is a context with no admission,
// which only a session this relay dialed has (a trusted peer).
func sessionGrant(ctx context.Context) (*auth.Grant, error) {
	a := admissionFrom(ctx)
	if a == nil {
		return nil, nil
	}
	select {
	case <-a.decided:
		return a.grant, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// authorizeSubscribe reports whether tw's session may subscribe to its path.
// A refusal closes tw with NotFound, the mux's answer for a path that doesn't
// exist, so a SUBSCRIBE alone doesn't tell a client which paths exist. Path
// names still reach it through announce interest and TRACK_INFO, which gomoqt
// answers inside the shared TrackMux (#418).
func authorizeSubscribe(tw *moqt.TrackWriter) bool {
	g, err := sessionGrant(tw.Context())
	switch {
	case err != nil:
		// The subscription ended before its session was admitted.
		return false
	case g == nil:
		return true
	case g.Subscribe.Contains(tw.BroadcastPath):
		metricSubscribeAuthorizations.WithLabelValues("admitted").Inc()
		return true
	}
	metricSubscribeAuthorizations.WithLabelValues("not_covered").Inc()
	slog.Info("relay: subscription refused: not covered by the session's grant",
		"broadcast_path", tw.BroadcastPath, "track_name", tw.TrackName)
	tw.CloseWithError(moqt.SubscribeErrorCodeNotFound)
	return false
}

// sessionCloser is the part of a session endAtDeadline closes.
type sessionCloser interface {
	CloseWithError(code moqt.SessionErrorCode, msg string) error
}

// endAtDeadline closes sess with Unauthorized and reason "expired" at
// deadline, the end of its grant; the client reconnects with a fresh
// credential. It returns nil for a zero deadline, which never expires;
// otherwise the caller stops the timer when the session ends first.
func endAtDeadline(sess sessionCloser, deadline time.Time) *time.Timer {
	if deadline.IsZero() {
		return nil
	}
	return time.AfterFunc(time.Until(deadline), func() {
		metricSessionsExpired.Inc()
		_ = sess.CloseWithError(moqt.UnauthorizedSessionErrorCode, "expired") // not actionable: the session is ending either way
	})
}

// newSessionID returns a random 128-bit hex id, unique per session.
func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return hex.EncodeToString(b[:])
}

// webTransportRequest describes a WebTransport upgrade for the auth server.
func (s *Server) webTransportRequest(r *http.Request) auth.Request {
	req := auth.Request{
		ID:        newSessionID(),
		Event:     auth.EventConnect,
		Node:      s.nodeID(),
		Transport: auth.TransportWebTransport,
		Remote:    r.RemoteAddr,
		Path:      r.URL.Path,
		Query:     r.URL.RawQuery,
	}
	if local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		req.Local = local.String()
	}
	if r.TLS != nil {
		req.ServerName = r.TLS.ServerName
	}
	return req
}

// nativeRequest describes a native-QUIC session for the auth server. Its path
// and query come from the SETUP Path parameter (gomoqt's Session.RequestURI),
// decoded the same way a WebTransport request's are.
func (s *Server) nativeRequest(sess *moqt.Session) auth.Request {
	req := auth.Request{
		ID:        newSessionID(),
		Event:     auth.EventConnect,
		Node:      s.nodeID(),
		Transport: auth.TransportQUIC,
		Path:      sess.RequestURI(),
	}
	if u, err := url.ParseRequestURI(sess.RequestURI()); err == nil {
		req.Path, req.Query = u.Path, u.RawQuery
	}
	if addr := sess.RemoteAddr(); addr != nil {
		req.Remote = addr.String()
	}
	if addr := sess.LocalAddr(); addr != nil {
		req.Local = addr.String()
	}
	if state := sess.ConnectionState().TLS; state != nil {
		req.ServerName = state.ServerName
	}
	return req
}

func (s *Server) nodeID() string {
	if s.Config == nil {
		return ""
	}
	return s.Config.NodeID
}

// admitUnchecked admits every session without asking anyone: the relay runs
// with auth off (QUMO_AUTH_URL unset). Its sessions are unchecked, like a
// trusted peer's.
func admitUnchecked(context.Context, auth.Request) (*auth.Grant, error) {
	return nil, nil
}

// errNoAuthorize refuses every client session of a Server whose Authorize is
// unset, rather than running it open.
var errNoAuthorize = errors.New("relay: no auth server configured")

// admit runs the connect check and records its outcome.
func (s *Server) admit(ctx context.Context, req auth.Request) (*auth.Grant, error) {
	if s.Authorize == nil {
		metricAuthRequests.WithLabelValues(auth.EventConnect, "error").Inc()
		slog.Error("relay: session refused: no auth server configured (Server.Authorize is nil)",
			"transport", req.Transport, "remote", req.Remote, "path", req.Path)
		return nil, errNoAuthorize
	}
	g, err := s.Authorize(ctx, req)
	_, refused := errors.AsType[auth.RefusedError](err)
	switch {
	case err == nil && g == nil:
		metricAuthRequests.WithLabelValues(auth.EventConnect, "unchecked").Inc()
	case err == nil:
		metricAuthRequests.WithLabelValues(auth.EventConnect, "admitted").Inc()
	case refused:
		metricAuthRequests.WithLabelValues(auth.EventConnect, "refused").Inc()
		slog.Info("relay: session refused", "transport", req.Transport, "remote", req.Remote,
			"path", req.Path, "reason", err)
	case errors.Is(err, auth.ErrInvalidGrant):
		metricAuthRequests.WithLabelValues(auth.EventConnect, "invalid").Inc()
		slog.Error("relay: session refused: the auth server's grant can't be enforced", "transport", req.Transport,
			"remote", req.Remote, "path", req.Path, "error", err)
	default:
		metricAuthRequests.WithLabelValues(auth.EventConnect, "error").Inc()
		slog.Error("relay: session refused: auth server unavailable", "transport", req.Transport,
			"remote", req.Remote, "path", req.Path, "error", err)
	}
	return g, err
}
