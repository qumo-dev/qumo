package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
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
	// credentials counts the credentials the session holds: its
	// connection's own, each presented again with no token, and its
	// tickets (ServeAuth).
	credentials atomic.Int32
	// published is set once the session is granted what may publish, by its
	// connection's credential or a ticket: a key paused for publishing still
	// renews its grant (Ticket).
	published atomic.Bool
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
// key that signed the session's own credential, and published when the
// session has been granted what may publish; Trusts while its grant is in
// force; and Publishing once a session is granted one that may publish.
type TicketVerifier interface {
	Ticket(credential, keyID string, published bool) (*auth.Grant, error)
	Trusts(g *auth.Grant) (bool, error)
	Publishing(id string)
}

const (
	// maxCredentials bounds the credentials one session may hold at once:
	// its connection's own and the tickets it presents, so a client can't
	// make the relay keep any number of them.
	maxCredentials = 33
	// ticketRevalidateEvery is how often a ticket in force is checked again,
	// so that a key leaving the set ends it, as it ends a session.
	ticketRevalidateEvery = 30 * time.Second
	// revokeMargin is how long a handler outlives its grant's MaxAge, so that
	// gomoqt revokes the grant first and tells the presenter: a handler that
	// returned first would end it silently.
	revokeMargin = time.Second
)

// everything is the grant of a session the relay checks nothing for: a relay
// peer, or any session with auth off.
var everything = moqt.Grant{Root: "/", Publish: []string{"/"}, Subscribe: []string{"/"}}

// ServeAuth answers a session's credentials (moqt.AuthHandler). The one its
// connection carried, presented with no token, is granted what the session
// was admitted with (admit.go), and a session checked for nothing is granted
// everything. A ticket presented on an Auth Stream adds its grant to the
// session's (serveTicket). Every grant of a verified credential lasts until
// its token expires, wherever the token was presented: a client that stays
// longer presents a fresh one before then.
//
// A client may present no token on an Auth Stream too, and gets its
// connection's grant again, for what is left of it, which adds nothing. A
// session holds at most maxCredentials credentials at once; those it
// presents first may crowd out its connection's own, which leaves only that
// client's session without its grant.
func (s *Server) ServeAuth(w *moqt.AuthWriter, r *moqt.AuthRequest) {
	if a := admissionFrom(r.Context()); a != nil {
		if a.credentials.Add(1) > maxCredentials {
			a.credentials.Add(-1)
			metricTickets.WithLabelValues(ticketRefused).Inc()
			slog.Info("relay: credential refused: too many held", "id", a.req.ID)
			_ = w.Refuse(moqt.UnauthorizedSessionErrorCode, "too many credentials")
			return
		}
		defer a.credentials.Add(-1)
	}
	if len(r.Token) > 0 {
		s.serveTicket(w, r)
		return
	}
	s.serveConnection(w, r)
}

// serveConnection grants the session's own credential, presented with no
// token: what it was admitted with, until the credential's token expires.
func (s *Server) serveConnection(w *moqt.AuthWriter, r *moqt.AuthRequest) {
	ctx := r.Context()
	g, err := sessionGrant(ctx)
	if err != nil {
		return // the session ended before its admission was decided
	}
	id := ""
	if a := admissionFrom(ctx); a != nil {
		id = a.req.ID
	}
	grant, lapses := everything, time.Time{}
	if g != nil {
		grant, lapses = moqGrant(g.Token()), lapsesAt(g)
	}
	if !lapses.IsZero() {
		left := time.Until(lapses)
		if left <= 0 {
			_ = w.Refuse(moqt.UnauthorizedSessionErrorCode, "credential expired")
			return
		}
		grant.MaxAge = left
	}
	// Marked before the grant is answered, so a ticket presented after the
	// answer finds it.
	if a := admissionFrom(ctx); a != nil && len(grant.Publish) > 0 {
		a.published.Store(true)
	}
	if err := w.Grant(grant); err != nil {
		slog.Error("relay: the session's grant could not be applied; it may do nothing", "id", id, "error", err)
		return
	}
	// The session's lease checks the credential again while it lives, and
	// ends the session when its key leaves the set.
	if hold(ctx, lapses, nil) == heldLapsed {
		slog.Info("relay: the session's credential expired", "id", id)
	}
}

// moqGrant is the MoQ grant a session is given for tg: a copy of its
// embedded one, or none at all when it has no publish or subscribe pattern,
// since a root alone would still let the session list announcements within
// it.
func moqGrant(tg token.Grant) moqt.Grant {
	if len(tg.Publish) == 0 && len(tg.Subscribe) == 0 {
		return moqt.Grant{}
	}
	return moqt.Grant{Root: tg.Root, Publish: slices.Clone(tg.Publish), Subscribe: slices.Clone(tg.Subscribe)}
}

// lapsesAt is when g's grant lapses: its token's expiry, judged with the
// leeway as token.Verify judges exp, or the zero time for a grant with no
// expiry.
func lapsesAt(g *auth.Grant) time.Time {
	if g.Expires().IsZero() {
		return time.Time{}
	}
	return g.Expires().Add(token.Leeway)
}

// How a grant held by hold ended.
type heldOutcome int

const (
	heldEnded   heldOutcome = iota // withdrawn, or the session ended
	heldLapsed                     // its MaxAge passed; gomoqt revoked it
	heldRevoked                    // revalidate refused it
)

// hold keeps a grant in force until ctx ends; until revokeMargin after
// lapses, so that gomoqt revokes it at its MaxAge first (a zero lapses never
// lapses); or until revalidate, called every ticketRevalidateEvery when set,
// reports it no longer trusted.
func hold(ctx context.Context, lapses time.Time, revalidate func() bool) heldOutcome {
	var lapsed <-chan time.Time
	if !lapses.IsZero() {
		lapse := time.NewTimer(time.Until(lapses) + revokeMargin)
		defer lapse.Stop()
		lapsed = lapse.C
	}
	var recheck <-chan time.Time
	if revalidate != nil {
		ticker := time.NewTicker(ticketRevalidateEvery)
		defer ticker.Stop()
		recheck = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return heldEnded
		case <-lapsed:
			return heldLapsed
		case <-recheck:
			if !revalidate() {
				return heldRevoked
			}
		}
	}
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

	tg, err := s.Tickets.Ticket(string(r.Token), g.KeyID(), a.published.Load())
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
	// AUTH_OK tells the presenter when it lapses, so it can present a fresh
	// ticket in time.
	lapses := lapsesAt(tg)
	grant := moqGrant(tg.Token())
	grant.MaxAge = max(time.Until(lapses), time.Millisecond)
	// Marked before the grant is answered, so a ticket presented after the
	// answer finds it. A key's pause refused this one already if it had to.
	if len(grant.Publish) > 0 {
		a.published.Store(true)
	}
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

	switch hold(ctx, lapses, func() bool {
		// A ticket that could not be checked just now stays in force, as a
		// session does, and is checked again.
		trusted, err := s.Tickets.Trusts(tg)
		return err != nil || trusted
	}) {
	case heldLapsed:
		// MaxAge has revoked it, telling the presenter so.
		metricTickets.WithLabelValues(ticketExpired).Inc()
	case heldRevoked:
		metricTickets.WithLabelValues(ticketRevoked).Inc()
		slog.Info("relay: ticket revoked: its key left the set", "id", a.req.ID, "kid", tg.KeyID())
		_ = w.Refuse(moqt.UnauthorizedSessionErrorCode, "ticket revoked")
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
