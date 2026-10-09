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
	"sync/atomic"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/token"
)

// admission is a session's grant, carried in the session's context so that
// both checks find it: announcements (serveSession) and subscriptions
// (authorizeSubscribe), whose TrackWriter context derives from the session's.
// A WebTransport session's is decided at the upgrade. A native-QUIC session's
// is pending from ConnContext until relayPeer decides it, and a subscription
// arriving before then waits.
type admission struct {
	decided chan struct{}
	// grant is set before decided closes. nil is unchecked: a relay peer, or
	// any session with auth off.
	grant *auth.Grant
	// internal marks an internal client (peer_trust.go): grant restricts
	// what it may do, but it has no credential to re-check and no usage to
	// report.
	internal bool
	// req is the connect request that admitted the session, set with
	// grant. A revalidate re-sends it, with the same id.
	req auth.Request
	// deadline is when the session must end, set with grant: its expires,
	// on the monotonic clock (see deadlineOf). Zero means never.
	deadline time.Time
	// served is set once serveSession runs the session, which then reports
	// its end. An admitted upgrade that fails never sets it.
	served atomic.Bool
}

type admissionKey struct{}

// refusedGrant is the grant of a refused session: it covers nothing.
var refusedGrant = &auth.Grant{}

// internalGrant is what an internal client may do: subscribe to anything,
// announce nothing.
var internalGrant = mustGrant(nil, []string{"**"})

func mustGrant(publish, subscribe []string) *auth.Grant {
	g, err := auth.NewGrant(publish, subscribe, time.Time{}, 0)
	if err != nil {
		panic(err) // fixed patterns, checked at init
	}
	return g
}

func pendingAdmission() *admission {
	return &admission{decided: make(chan struct{})}
}

func decidedAdmission(g *auth.Grant, req auth.Request) *admission {
	a := pendingAdmission()
	a.decide(g, req)
	return a
}

// decide records the outcome of the connect request req: g, nil for an
// unchecked session or refusedGrant for a refused one.
func (a *admission) decide(g *auth.Grant, req auth.Request) {
	a.grant = g
	a.req = req
	a.deadline = deadlineOf(g)
	close(a.decided)
}

// decideInternal records an internal client's admission: internalGrant,
// with no connect request behind it.
func (a *admission) decideInternal() {
	a.internal = true
	a.decide(internalGrant, auth.Request{})
}

// deadlineOf returns when a session holding g must end: its expires, taken
// now on the monotonic clock so that a wall-clock jump doesn't move it.
// time.Now carries a monotonic reading and Add keeps it. Zero means never.
func deadlineOf(g *auth.Grant) time.Time {
	if g == nil || g.Expires().IsZero() {
		return time.Time{}
	}
	return time.Now().Add(time.Until(g.Expires()))
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
// which only a session this relay dialed has (a relay peer).
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

// authorizeSubscribe reports whether tw's session may subscribe to its track.
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
	case admissionFrom(tw.Context()).internal:
		// An internal client's grant covers every path and came from no
		// credential: there is nothing to count as an authorization.
		return true
	case g.Allows(token.ActionSubscribe, tw.BroadcastPath, tw.TrackName):
		metricSubscribeAuthorizations.WithLabelValues("admitted").Inc()
		return true
	}
	metricSubscribeAuthorizations.WithLabelValues("not_covered").Inc()
	slog.Info("relay: subscription refused: not covered by the session's grant",
		"broadcast_path", tw.BroadcastPath, "track_name", tw.TrackName)
	tw.CloseWithError(moqt.SubscribeErrorCodeNotFound)
	return false
}

// newSessionID returns a random 128-bit hex id, unique per session.
func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return hex.EncodeToString(b[:])
}

// webTransportRequest describes a WebTransport upgrade for Authorize.
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

// nativeRequest describes a native-QUIC session for Authorize. Its path
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
// with auth off (QUMO_AUTH_KEYS unset). Its sessions are unchecked, like a
// relay peer's.
func admitUnchecked(context.Context, auth.Request) (*auth.Grant, error) {
	return nil, nil
}

// errNoAuthorize refuses every client session of a Server whose Authorize is
// unset, rather than running it open.
var errNoAuthorize = errors.New("relay: no Authorize configured")

// Outcomes of an auth request, the result label of metricAuthRequests. An
// end report is ok or error.
const (
	authUnchecked = "unchecked"
	authAdmitted  = "admitted"
	authRefused   = "refused"
	authError     = "error"
	authOK        = "ok"
)

// authOutcome classifies an Authorize reply.
func authOutcome(g *auth.Grant, err error) string {
	_, refused := errors.AsType[auth.RefusedError](err)
	switch {
	case err == nil && g == nil:
		return authUnchecked
	case err == nil:
		return authAdmitted
	case refused:
		return authRefused
	}
	return authError
}

// admit runs the connect check and records its outcome.
func (s *Server) admit(ctx context.Context, req auth.Request) (*auth.Grant, error) {
	if s.Authorize == nil {
		metricAuthRequests.WithLabelValues(auth.EventConnect, authError).Inc()
		slog.Error("relay: session refused: Server.Authorize is nil",
			"transport", req.Transport, "remote", req.Remote, "path", req.Path)
		return nil, errNoAuthorize
	}
	g, err := s.Authorize(ctx, req)
	outcome := authOutcome(g, err)
	metricAuthRequests.WithLabelValues(auth.EventConnect, outcome).Inc()
	switch outcome {
	case authRefused:
		slog.Info("relay: session refused", "transport", req.Transport, "remote", req.Remote,
			"path", req.Path, "reason", err)
	case authError:
		slog.Error("relay: session refused: it could not be checked", "transport", req.Transport,
			"remote", req.Remote, "path", req.Path, "error", err)
	}
	return g, err
}

// reportEnd sends the end event of a checked session admitted by req, with
// its final byte totals, why it ended and how long it lasted. ctx is the
// session's: done by now, so the report keeps its values but not its
// cancellation. A failed report is recorded and otherwise ignored; End
// bounds its own wait.
func (s *Server) reportEnd(ctx context.Context, req auth.Request, stats moqt.SessionStats, reason string, d time.Duration) {
	if s.End == nil {
		return
	}
	req.Event = auth.EventEnd
	req.Bytes = sessionBytes(stats)
	req.Reason = reason
	req.Duration = int64(d / time.Second)
	if err := s.End(context.WithoutCancel(ctx), req); err != nil {
		metricAuthRequests.WithLabelValues(auth.EventEnd, authError).Inc()
		slog.Warn("relay: end report failed", "id", req.ID, "reason", reason, "error", err)
		return
	}
	metricAuthRequests.WithLabelValues(auth.EventEnd, authOK).Inc()
}
