package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/token"
)

// admission is a session's grant, carried in the session's context so that
// its credentials' answers find it (ServeAuth): gomoqt enforces what they
// grant. A WebTransport session's is decided at the upgrade. A native-QUIC
// session's is pending from ConnContext until relayPeer decides it, and the
// session serves no request before then.
type admission struct {
	decided chan struct{}
	// grant is set before decided closes. nil is unchecked: a relay peer, or
	// any session with auth off.
	grant *auth.Grant
	// req is the connect request that admitted the session, set with
	// grant. A revalidate re-sends it, with the same id.
	req auth.Request
	// served is set once serveSession runs the session, which then reports
	// its end. An admitted upgrade that fails never sets it.
	served atomic.Bool
	// refusal is why a session that was refused at connect, and upgraded
	// all the same, is to be closed: refusalRefused or refusalUnavailable.
	// Only a WebSocket session has one (handleWebSocket).
	refusal string
	// client is what the upgrade request said about its client, set before
	// the session starts. Zero for a native-QUIC session.
	client upgradeClient
	// tickets counts the tickets the session holds, and plain the
	// credentials presented with no token, its connection's own among them
	// (ServeAuth).
	tickets, plain atomic.Int32
}

type admissionKey struct{}

// refusedGrant is the grant of a refused session: it covers nothing.
var refusedGrant = &auth.Grant{}

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

// newSessionID returns a random 128-bit hex id, unique per session.
func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return hex.EncodeToString(b[:])
}

// upgradeRequest describes an HTTP upgrade, to WebTransport or to
// WebSocket, for Authorize. Both carry the path and the credential in the
// request URI, so a session is described the same on either.
func (s *Server) upgradeRequest(r *http.Request, transport string) auth.Request {
	return auth.Request{
		ID:        newSessionID(),
		Event:     auth.EventConnect,
		Transport: transport,
		Remote:    r.RemoteAddr,
		Path:      r.URL.Path,
		Query:     r.URL.RawQuery,
	}
}

// nativeRequest describes a native-QUIC session for Authorize. Its path
// and query come from the SETUP Path parameter (gomoqt's Session.RequestURI),
// decoded the same way a WebTransport request's are.
func (s *Server) nativeRequest(sess *moqt.Session) auth.Request {
	req := auth.Request{
		ID:        newSessionID(),
		Event:     auth.EventConnect,
		Transport: auth.TransportQUIC,
		Path:      sess.RequestURI(),
	}
	if u, err := url.ParseRequestURI(sess.RequestURI()); err == nil {
		req.Path, req.Query = u.Path, u.RawQuery
	}
	if addr := sess.RemoteAddr(); addr != nil {
		req.Remote = addr.String()
	}
	return req
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
// its final byte totals and why it ended. ctx is the session's: done by now,
// so the report keeps its values but not its cancellation. A failed report is
// recorded and otherwise ignored; End bounds its own wait.
func (s *Server) reportEnd(ctx context.Context, req auth.Request, stats moqt.SessionStats, reason string) {
	if s.End == nil {
		return
	}
	req.Event = auth.EventEnd
	req.Bytes = sessionBytes(stats)
	req.Reason = reason
	if err := s.End(context.WithoutCancel(ctx), req); err != nil {
		metricAuthRequests.WithLabelValues(auth.EventEnd, authError).Inc()
		slog.Warn("relay: end report failed", "id", req.ID, "reason", reason, "error", err)
		return
	}
	metricAuthRequests.WithLabelValues(auth.EventEnd, authOK).Inc()
}

// sessionTransport names what a session runs over, for the metrics and the
// log: what its connect request said. A session admitted without one, a
// relay peer, runs over native QUIC.
func sessionTransport(a *admission) string {
	if a != nil && a.req.Transport != "" {
		return a.req.Transport
	}
	return auth.TransportQUIC
}

// admittedClass is the class a session was admitted as, for the metrics and
// the log. A client has a connect request. A relay peer has none: neither
// an inbound one, nor one this relay dialed, which has no admission either.
func admittedClass(a *admission) sessionClass {
	if a != nil && a.req.Transport != "" {
		return classClient
	}
	return classPeer
}

// Reasons a session refused at connect is closed with.
const (
	// refusalRefused is a credential that was not accepted: retrying with
	// it will not help.
	refusalRefused = "refused"
	// refusalUnavailable is a session that could not be checked just now.
	refusalUnavailable = "unavailable"
)

// refusalReason is the close reason for a session refused with err.
func refusalReason(err error) string {
	if _, refused := errors.AsType[auth.RefusedError](err); refused {
		return refusalRefused
	}
	return refusalUnavailable
}

// TicketVerifier verifies the tickets a session presents on Auth Streams
// (auth.Verifier): Ticket when one is presented, which must be signed by the
// key that signed the session's own credential; Trusts while its grant is in
// force; and Publishing once a session is granted one that may publish.
type TicketVerifier interface {
	Ticket(credential, keyID string) (*auth.Grant, error)
	Trusts(g *auth.Grant) (bool, error)
	Publishing(id string)
}

const (
	// maxTickets bounds the tickets one session may hold at once, so a
	// client can't make the relay keep any number of them. The connection's
	// own credential is not one of them.
	maxTickets = 32
	// ticketRevalidateEvery is how often a ticket in force is checked again,
	// so that a key leaving the set ends it, as it ends a session.
	ticketRevalidateEvery = 30 * time.Second
)

// everything is the grant of a session the relay checks nothing for: a relay
// peer, or any session with auth off.
var everything = moqt.Grant{Root: "/", Publish: []string{"/"}, Subscribe: []string{"/"}}

// ServeAuth answers a session's credentials (moqt.AuthHandler). The one its
// connection carried, presented with no token, is granted what the session
// was admitted with (admit.go), and a session checked for nothing is granted
// everything. A ticket presented on an Auth Stream adds its grant to the
// session's, while it is in force (serveTicket).
//
// A client may present no token on an Auth Stream too, and gets the same
// grant again, which adds nothing. A session holds at most maxTickets
// tickets, and as many credentials of no token beside its connection's own;
// those it presents first may crowd out its connection's own, which leaves
// only that client's session without its grant.
func (s *Server) ServeAuth(w *moqt.AuthWriter, r *moqt.AuthRequest) {
	if a := admissionFrom(r.Context()); a != nil {
		held, limit := &a.plain, int32(maxTickets+1)
		if len(r.Token) > 0 {
			held, limit = &a.tickets, maxTickets
		}
		if held.Add(1) > limit {
			held.Add(-1)
			metricTickets.WithLabelValues(ticketRefused).Inc()
			slog.Info("relay: credential refused: too many held", "id", a.req.ID)
			_ = w.Refuse(moqt.UnauthorizedSessionErrorCode, "too many tickets")
			return
		}
		defer held.Add(-1)
	}
	if len(r.Token) > 0 {
		s.serveTicket(w, r)
		return
	}
	g, err := sessionGrant(r.Context())
	if err != nil {
		return // the session ended before its admission was decided
	}
	if err := w.Grant(connectionGrant(g)); err != nil {
		id := ""
		if a := admissionFrom(r.Context()); a != nil {
			id = a.req.ID
		}
		slog.Error("relay: the session's grant could not be applied; it may do nothing", "id", id, "error", err)
		return
	}
	<-r.Context().Done()
}

// connectionGrant is what a session admitted with g may do: what g grants,
// or everything for a session checked for nothing (nil).
func connectionGrant(g *auth.Grant) moqt.Grant {
	if g == nil {
		return everything
	}
	return g.MoQ()
}

// serveTicket verifies a ticket and grants it until it expires, its presenter
// withdraws it or its key leaves the set. Only a session admitted with a
// credential the relay verified may present one, and it must be signed by
// the same key.
func (s *Server) serveTicket(w *moqt.AuthWriter, r *moqt.AuthRequest) {
	ctx := r.Context()
	a := admissionFrom(ctx)
	g, err := sessionGrant(ctx)
	if err != nil {
		return
	}
	if s.Tickets == nil || g == nil || g.KeyID() == "" {
		metricTickets.WithLabelValues(ticketNotSupported).Inc()
		_ = w.Refuse(moqt.NotSupportedErrorCode, "this relay verifies no tickets for the session")
		return
	}

	tg, err := s.Tickets.Ticket(string(r.Token), g.KeyID())
	if err != nil {
		if _, refused := errors.AsType[auth.RefusedError](err); !refused {
			// Not the ticket's fault: the key set is not usable just now.
			metricTickets.WithLabelValues(ticketError).Inc()
			slog.Warn("relay: ticket could not be checked", "id", a.req.ID, "error", err)
			_ = w.Refuse(moqt.InternalSessionErrorCode, "ticket could not be checked; present it again")
			return
		}
		metricTickets.WithLabelValues(ticketRefused).Inc()
		slog.Info("relay: ticket refused", "id", a.req.ID, "reason", err)
		_ = w.Refuse(moqt.UnauthorizedSessionErrorCode, "ticket refused")
		return
	}
	// Judged with the leeway, as token.Verify judges exp. AUTH_OK tells the
	// presenter when, so it can present a fresh ticket in time.
	expires := tg.Expires().Add(token.Leeway)
	grant := tg.MoQ()
	grant.MaxAge = max(time.Until(expires), time.Millisecond)
	if err := w.Grant(grant); err != nil {
		metricTickets.WithLabelValues(ticketRefused).Inc()
		slog.Info("relay: ticket not granted", "id", a.req.ID, "error", err)
		// A grant gomoqt refused itself, too large, is answered already.
		if !errors.Is(err, moqt.ErrAuthEnded) {
			_ = w.Refuse(moqt.UnauthorizedSessionErrorCode, "ticket grants what this relay can't apply")
		}
		return
	}
	metricTickets.WithLabelValues(ticketGranted).Inc()
	if len(grant.Publish) > 0 {
		s.Tickets.Publishing(a.req.ID)
	}

	// A second after MaxAge, so gomoqt revokes the grant first and tells the
	// presenter: a handler that returned first would end it silently.
	lapse := time.NewTimer(time.Until(expires) + time.Second)
	defer lapse.Stop()
	revalidate := time.NewTicker(ticketRevalidateEvery)
	defer revalidate.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-lapse.C:
			// MaxAge has revoked it, telling the presenter so.
			metricTickets.WithLabelValues(ticketExpired).Inc()
			return
		case <-revalidate.C:
			trusted, err := s.Tickets.Trusts(tg)
			if err == nil && !trusted {
				metricTickets.WithLabelValues(ticketRevoked).Inc()
				slog.Info("relay: ticket revoked: its key left the set", "id", a.req.ID, "kid", tg.KeyID())
				_ = w.Refuse(moqt.UnauthorizedSessionErrorCode, "ticket revoked")
				return
			}
			// A ticket that could not be checked just now stays in force, as
			// a session does, and is checked again.
		}
	}
}

// Outcomes of a ticket, the result label of metricTickets.
const (
	ticketGranted      = "granted"
	ticketRefused      = "refused"
	ticketRevoked      = "revoked"
	ticketExpired      = "expired"
	ticketError        = "error"
	ticketNotSupported = "not_supported"
)
