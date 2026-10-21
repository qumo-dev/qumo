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
	"sync"
	"sync/atomic"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/token"
	"github.com/qumo-dev/qumo/usage"
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
	// held is the grants the session holds, and for an anonymous one its
	// key and deadline.
	held heldGrants
	// roots keeps the session's credentials for each root.
	roots rootHolds
}

// rootHolds keeps, for each root, the session's credentials in force for it:
// a ticket for a root the session holds a ticket for replaces it, so that
// renewing a grant may narrow it, and the first one for the root of the
// connection's own credential retires that. Credentials for different roots
// add up.
//
// A ticket's grant is applied before it is registered here, outside mu, so
// it is in force before the credential it replaces is withdrawn, and a grant
// that stalls on its stream holds up no other credential of the session.
// Tickets for a root are ordered by when their grants began (seq): one that
// finishes after a later one is withdrawn as soon as it is registered.
type rootHolds struct {
	mu sync.Mutex
	// seq numbers the tickets whose grants began.
	seq uint64
	// byRoot is the credentials in force for each root that has any.
	byRoot map[string]*rootState
	// conns withdraws each presentation of the connection's own credential
	// in force.
	conns map[*withdrawal]struct{}
	// connRetired is set once a ticket for the root of the connection's own
	// credential was granted: that credential is not granted again.
	connRetired bool
}

// rootState is the tickets in force for one root.
type rootState struct {
	// ticket is the one that replaces others: it grants relay patterns, or
	// nothing at all.
	ticket *withdrawal
	// funnel is the tickets of funnel actions alone, which add up beside it.
	funnel map[*withdrawal]struct{}
	// cleared is the seq of the latest ticket of no patterns, which
	// withdrew every ticket for the root begun before it.
	cleared uint64
}

func (st *rootState) empty() bool {
	return st.ticket == nil && len(st.funnel) == 0
}

// withdrawal ends a credential's grant, with its cause.
type withdrawal struct {
	withdraw context.CancelCauseFunc
	seq      uint64
}

// errSuperseded is why a credential was withdrawn: a later ticket for its
// root is in force in its place.
var errSuperseded = errors.New("relay: superseded by a later ticket for its root")

// rootRole is what a ticket does to the session's other tickets for its
// root.
type rootRole int

const (
	// roleNone is a ticket of no root: it touches nothing.
	roleNone rootRole = iota
	// roleFunnel grants funnel actions alone, which grant nothing on the
	// relay: it adds up beside the root's other tickets.
	roleFunnel
	// roleRelay grants relay patterns: it replaces the root's ticket of
	// relay patterns or of none.
	roleRelay
	// roleNothing grants no pattern at all: it narrows the root to nothing,
	// replacing every ticket for it.
	roleNothing
)

// roleOf is the role of a ticket granting tg.
func roleOf(tg token.Grant) rootRole {
	switch {
	case tg.Root == "":
		return roleNone
	case len(tg.Publish) > 0 || len(tg.Subscribe) > 0:
		return roleRelay
	case len(tg.Create) > 0 || len(tg.Post) > 0 || len(tg.Redact) > 0:
		return roleFunnel
	default:
		return roleNothing
	}
}

// beginTicketGrant numbers a ticket whose grant is about to be applied, for
// registerTicket.
func (a *admission) beginTicketGrant() uint64 {
	a.roots.mu.Lock()
	defer a.roots.mu.Unlock()
	a.roots.seq++
	return a.roots.seq
}

// registerTicket registers a ticket for root of role whose grant, begun as
// seq, is in force, with withdraw to end it. It withdraws what the ticket
// replaces, and every presentation of the connection's own credential when
// root is connRoot (that credential's root), retiring it. A ticket that a
// ticket begun after it replaced already is withdrawn at once. release
// forgets the ticket when it ends.
func (a *admission) registerTicket(root, connRoot string, role rootRole, seq uint64,
	withdraw context.CancelCauseFunc) (release func()) {
	if role == roleNone {
		return func() {}
	}
	r := &a.roots
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.byRoot[root]
	if st == nil {
		st = &rootState{funnel: make(map[*withdrawal]struct{})}
		r.byRoot[root] = st
	}
	if role != roleFunnel && root == connRoot {
		r.connRetired = true
		for c := range r.conns {
			c.withdraw(errSuperseded)
		}
		clear(r.conns)
	}
	h := &withdrawal{withdraw: withdraw, seq: seq}
	switch {
	case seq < st.cleared, role != roleFunnel && st.ticket != nil && seq < st.ticket.seq:
		// A ticket begun after this one replaced it already.
		withdraw(errSuperseded)
		if st.empty() {
			delete(r.byRoot, root)
		}
		return func() {}
	case role == roleFunnel:
		st.funnel[h] = struct{}{}
	default:
		if st.ticket != nil {
			st.ticket.withdraw(errSuperseded)
		}
		st.ticket = h
		if role == roleNothing {
			for f := range st.funnel {
				f.withdraw(errSuperseded)
			}
			clear(st.funnel)
			st.cleared = seq
		}
	}
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		cur, ok := r.byRoot[root]
		if !ok {
			return
		}
		if cur.ticket == h {
			cur.ticket = nil
		}
		delete(cur.funnel, h)
		if cur.empty() {
			delete(r.byRoot, root)
		}
	}
}

// registerConnection registers a presentation of the connection's own
// credential, before its grant is applied, so that a ticket for its root
// granted from then on withdraws it with withdraw; once one was, it is
// refused (false). Presentations of the credential add up, as they grant the
// same. release forgets it when it ends.
func (a *admission) registerConnection(withdraw context.CancelCauseFunc) (release func(), ok bool) {
	r := &a.roots
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.connRetired {
		return nil, false
	}
	h := &withdrawal{withdraw: withdraw}
	r.conns[h] = struct{}{}
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.conns, h)
	}, true
}

// heldGrants is what a session's grants in force decide about it. An
// anonymous session (one that connected with no credential) is bound to the
// key of the first ticket granted on it, and is closed whenever it has held
// no grant that grants something for its deadline.
type heldGrants struct {
	mu sync.Mutex
	// key is the kid an anonymous session is bound to, or "" while none
	// is. A ticket in flight binds it tentatively: the binding holds once a
	// grant succeeds, and is undone when every ticket in flight failed.
	key string
	// opened is set once a grant on an anonymous session succeeded: its
	// usage is open under key, which binds it for good.
	opened bool
	// publishes is set once usage reports the anonymous session as a
	// publisher.
	publishes bool
	// inFlight counts tickets that passed the key check and are being
	// granted.
	inFlight int
	// live counts the grants in force that grant something.
	live int
	// pending is the server's count of anonymous sessions that hold no
	// grant yet, this one among them until its first grant or its end, or
	// nil once it is not.
	pending *atomic.Int64
	// wait is how long an anonymous session may hold no grant, and expire
	// closes it then; both are set while its deadline is watched (watch).
	wait   time.Duration
	expire func()
	timer  *time.Timer
	// closedFor is why the relay closed the session, or "".
	closedFor string
}

// boundKey returns the kid an anonymous session is bound to, or "" while no
// ticket has bound it.
func (a *admission) boundKey() string {
	a.held.mu.Lock()
	defer a.held.mu.Unlock()
	return a.held.key
}

// beginTicket binds an anonymous session to kid, the key of a ticket that
// verified, while the ticket is granted: the first key wins, and a ticket of
// any other key is refused (false). Every true is followed by settleTicket.
func (a *admission) beginTicket(kid string) bool {
	h := &a.held
	h.mu.Lock()
	defer h.mu.Unlock()
	switch h.key {
	case "":
		h.key = kid
	case kid:
	default:
		return false
	}
	h.inFlight++
	return true
}

// settleTicket records whether a ticket begun by beginTicket was granted,
// and whether what it grants publishes. The first granted on an anonymous
// session opens its usage (open, with whether it publishes) and binds the
// session for good; a later one that publishes makes it a publisher
// (publishing), always after the open whatever order the grants came in. A
// binding no grant ever held is undone once no ticket is in flight.
func (a *admission) settleTicket(granted, publishes bool, open func(publishes bool), publishing func()) {
	h := &a.held
	h.mu.Lock()
	defer h.mu.Unlock()
	h.inFlight--
	switch {
	case !granted:
		if !h.opened && h.inFlight == 0 {
			h.key = ""
		}
	case !h.opened:
		h.opened, h.publishes = true, publishes
		open(publishes)
	case publishes && !h.publishes:
		h.publishes = true
		publishing()
	}
}

// countPending counts the session in pending, the server's anonymous
// sessions that hold no grant yet, until its first grant or its end.
func (a *admission) countPending(pending *atomic.Int64) {
	a.held.mu.Lock()
	defer a.held.mu.Unlock()
	a.held.pending = pending
}

// releasePendingLocked takes the session out of the server's pending
// anonymous sessions. The caller holds held.mu.
func (h *heldGrants) releasePendingLocked() {
	if h.pending != nil {
		h.pending.Add(-1)
		h.pending = nil
	}
}

// holding records a grant in force that grants something, until released.
func (a *admission) holding() {
	h := &a.held
	h.mu.Lock()
	defer h.mu.Unlock()
	h.live++
	h.releasePendingLocked()
	if h.timer != nil {
		h.timer.Stop()
	}
}

// released records that a grant holding recorded has ended. An anonymous
// session left with none has its deadline again.
func (a *admission) released() {
	h := &a.held
	h.mu.Lock()
	defer h.mu.Unlock()
	h.live--
	if h.live == 0 && h.expire != nil {
		h.armLocked()
	}
}

// watch closes the anonymous session with expire whenever it has held no
// grant that grants something for wait, until unwatch.
func (a *admission) watch(wait time.Duration, expire func()) {
	h := &a.held
	h.mu.Lock()
	defer h.mu.Unlock()
	h.wait, h.expire = wait, expire
	if h.live == 0 {
		h.armLocked()
	}
}

// armLocked starts the deadline. The caller holds held.mu.
func (h *heldGrants) armLocked() {
	if h.timer != nil {
		h.timer.Stop()
	}
	var timer *time.Timer
	timer = time.AfterFunc(h.wait, func() {
		h.mu.Lock()
		if h.timer != timer || h.live > 0 || h.expire == nil {
			h.mu.Unlock()
			return
		}
		h.closedFor = endUnbound
		expire := h.expire
		h.expire = nil
		h.mu.Unlock()
		expire()
	})
	h.timer = timer
}

// unwatch stops the deadline and takes the session out of the pending
// ones, as the session ends.
func (a *admission) unwatch() {
	h := &a.held
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expire = nil
	if h.timer != nil {
		h.timer.Stop()
	}
	h.releasePendingLocked()
}

// closedReason returns why the relay closed the session itself, or "".
func (a *admission) closedReason() string {
	a.held.mu.Lock()
	defer a.held.mu.Unlock()
	return a.held.closedFor
}

type admissionKey struct{}

// refusedGrant is the grant of a refused session: it covers nothing.
var refusedGrant = &auth.Grant{}

func pendingAdmission() *admission {
	return &admission{
		decided: make(chan struct{}),
		roots: rootHolds{
			byRoot: make(map[string]*rootState),
			conns:  make(map[*withdrawal]struct{}),
		},
	}
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
	// authBusy is an anonymous session refused while too many others hold
	// no grant yet.
	authBusy = "busy"
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

// errTooManyPending refuses an anonymous session as unavailable while
// maxPendingAnonymous others hold no grant yet.
var errTooManyPending = errors.New("relay: too many sessions hold no credential yet")

// reservePending counts an anonymous session being admitted among those
// that hold no grant yet, or reports false when there are as many as the
// relay takes. The admission it gets takes it out again (countPending).
func (s *Server) reservePending() bool {
	if s.pendingAnonymous.Add(1) > int64(s.maxPendingAnonymous()) {
		s.pendingAnonymous.Add(-1)
		return false
	}
	return true
}

// maxPendingAnonymous is Server.MaxPendingAnonymous, or its default.
func (s *Server) maxPendingAnonymous() int {
	if s.MaxPendingAnonymous <= 0 {
		return maxPendingAnonymous
	}
	return s.MaxPendingAnonymous
}

// authDeadline is Server.AuthDeadline, or its default.
func (s *Server) authDeadline() time.Duration {
	if s.AuthDeadline <= 0 {
		return authDeadline
	}
	return s.AuthDeadline
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
	if outcome == authAdmitted && g.Anonymous() && !s.reservePending() {
		g, err, outcome = nil, errTooManyPending, authBusy
	}
	metricAuthRequests.WithLabelValues(auth.EventConnect, outcome).Inc()
	switch outcome {
	case authBusy:
		slog.Warn("relay: session refused: too many sessions hold no credential yet", "transport", req.Transport,
			"remote", req.Remote, "path", req.Path, "limit", s.maxPendingAnonymous())
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
// (auth.Verifier): Ticket when one is presented, which must be signed by
// keyID, the key that signed the session's own credential or that an
// anonymous session is bound to, and published when the session has been
// granted what may publish; FirstTicket for the first ticket of an
// anonymous session no key binds yet, which any trusted key may sign;
// Trusts while its grant is in force; Bind once a ticket granted on an
// anonymous session binds it to its key, with whether it publishes; and
// Publishing once a session is granted one that may publish.
type TicketVerifier interface {
	Ticket(credential, keyID string, published bool) (*auth.Grant, error)
	FirstTicket(credential string) (*auth.Grant, error)
	Trusts(g *auth.Grant) (bool, error)
	Bind(id, keyID string, publishes bool)
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
	// authDeadline is how long a session that connected with no credential
	// may hold no grant that grants something before the relay closes it,
	// so that one can't hold a session open granted nothing.
	authDeadline = 10 * time.Second
	// maxPendingAnonymous is how many sessions that connected with no
	// credential and hold no grant yet may be open at once; past it the
	// relay refuses another as unavailable.
	maxPendingAnonymous = 1024
	// endUnbound is why an anonymous session was closed at its deadline,
	// and endUnboundMessage the close message its client sees.
	endUnbound        = usage.ReasonUnauthenticated
	endUnboundMessage = "no credential presented in time"
)

// everything is the grant of a session the relay checks nothing for: a relay
// peer, or any session with auth off.
var everything = moqt.Grant{Root: "/", Publish: []string{"/"}, Subscribe: []string{"/"}}

// ServeAuth answers a session's credentials (moqt.AuthHandler). The one its
// connection carried, presented with no token, is granted what the session
// was admitted with (admit.go), and a session checked for nothing is granted
// everything. A ticket presented on an Auth Stream adds its grant to the
// session's (serveTicket), or replaces the session's credentials for the
// same root (rootHolds): once its grant is in force, the earlier ticket for
// that root and the connection's own credential, if that is the root's, are
// withdrawn, answered AUTH_ERROR with NO_ERROR, so a renewal may narrow what
// the session may do. A connection credential a ticket replaced is not
// granted again. Every grant of a verified credential
// lasts until its token expires, wherever the token was presented: a client
// that stays longer presents a fresh one before then.
//
// A session that connected with no credential (an anonymous one) is granted
// nothing for it, and holds what its tickets grant. Whenever it holds no
// grant that grants something, at its start or once its tickets have ended,
// it is closed after authDeadline unless one is granted meanwhile
// (serveSession).
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
// token: what it was admitted with, until the credential's token expires or
// a ticket for its root supersedes it.
func (s *Server) serveConnection(w *moqt.AuthWriter, r *moqt.AuthRequest) {
	ctx := r.Context()
	g, err := sessionGrant(ctx)
	if err != nil {
		return // the session ended before its admission was decided
	}
	a := admissionFrom(ctx)
	id := ""
	if a != nil {
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
	ctx, withdraw := context.WithCancelCause(ctx)
	defer withdraw(nil)
	if a != nil && g != nil && g.Token().Root != "" {
		// Registered before its grant is applied, so that a ticket for its
		// root granted meanwhile withdraws it.
		release, ok := a.registerConnection(withdraw)
		if !ok {
			// not actionable: the credential ends either way.
			_ = w.Refuse(moqt.UnauthorizedSessionErrorCode, "credential superseded by a ticket")
			return
		}
		defer release()
	}
	// Marked before the grant is answered, so a ticket presented after the
	// answer finds it.
	if a != nil && len(grant.Publish) > 0 {
		a.published.Store(true)
	}
	if err = w.Grant(grant); err != nil {
		slog.Error("relay: the session's grant could not be applied; it may do nothing", "id", id, "error", err)
		return
	}
	if a != nil && grantsSomething(grant) {
		a.holding()
		defer a.released()
	}
	// The session's lease checks the credential again while it lives, and
	// ends the session when its key leaves the set.
	switch hold(ctx, lapses, nil) {
	case heldLapsed:
		slog.Info("relay: the session's credential expired", "id", id)
	case heldEnded:
		if errors.Is(context.Cause(ctx), errSuperseded) {
			slog.Debug("relay: the session's credential was superseded by a ticket", "id", id)
			// not actionable: the grant ends either way, and a failure to
			// tell the presenter is its stream failing.
			_ = w.Refuse(moqt.NoError, "superseded by a ticket for its root")
		}
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

// grantsSomething reports whether g lets a session publish or subscribe to
// anything.
func grantsSomething(g moqt.Grant) bool {
	return len(g.Publish) > 0 || len(g.Subscribe) > 0
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
// withdraws it, a later credential for its root supersedes it or its key
// leaves the set. Once granted, it supersedes the session's earlier
// credential for its root. Only a session admitted with a credential the
// relay verified, or anonymous, may present one. It must be
// signed by the key of the session's credential or, for an anonymous
// session, by the key its first ticket bound it to.
func (s *Server) serveTicket(w *moqt.AuthWriter, r *moqt.AuthRequest) {
	ctx := r.Context()
	a := admissionFrom(ctx)
	g, err := sessionGrant(ctx)
	if err != nil {
		return
	}
	if s.Tickets == nil || g == nil || (g.KeyID() == "" && !g.Anonymous()) {
		metricTickets.WithLabelValues(ticketNotSupported).Inc()
		_ = w.Refuse(moqt.NotSupportedErrorCode, "this relay verifies no tickets for the session")
		return
	}

	anonymous := g.Anonymous()
	var tg *auth.Grant
	switch key := a.boundKey(); {
	case !anonymous:
		tg, err = s.Tickets.Ticket(string(r.Token), g.KeyID(), a.published.Load())
	case key != "":
		tg, err = s.Tickets.Ticket(string(r.Token), key, a.published.Load())
	default:
		tg, err = s.Tickets.FirstTicket(string(r.Token))
	}
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
	if anonymous && !a.beginTicket(tg.KeyID()) {
		// Another ticket bound the session to its key first.
		metricTickets.WithLabelValues(ticketRefused).Inc()
		slog.Info("relay: ticket refused: the session is bound to another key", "id", a.req.ID, "kid", tg.KeyID())
		_ = w.Refuse(moqt.UnauthorizedSessionErrorCode, "ticket refused")
		return
	}
	// AUTH_OK tells the presenter when it lapses, so it can present a fresh
	// ticket in time.
	lapses := lapsesAt(tg)
	grant := moqGrant(tg.Token())
	grant.MaxAge = max(time.Until(lapses), time.Millisecond)
	seq := a.beginTicketGrant()
	// Marked before the grant is answered, so a ticket presented after the
	// answer finds it. A key's pause refused this one already if it had to.
	if len(grant.Publish) > 0 {
		a.published.Store(true)
	}
	err = w.Grant(grant)
	publishes := len(grant.Publish) > 0
	if anonymous {
		a.settleTicket(err == nil, publishes,
			func(publishes bool) { s.Tickets.Bind(a.req.ID, tg.KeyID(), publishes) },
			func() { s.Tickets.Publishing(a.req.ID) })
	}
	if err != nil {
		metricTickets.WithLabelValues(ticketRefused).Inc()
		slog.Info("relay: ticket not granted", "id", a.req.ID, "error", err)
		// A grant gomoqt refused itself, too large, is answered already.
		if !errors.Is(err, moqt.ErrAuthEnded) {
			_ = w.Refuse(moqt.UnauthorizedSessionErrorCode, "ticket grants what this relay can't apply")
		}
		return
	}
	metricTickets.WithLabelValues(ticketGranted).Inc()
	if publishes && !anonymous {
		s.Tickets.Publishing(a.req.ID)
	}
	if grantsSomething(grant) {
		a.holding()
		defer a.released()
	}
	// Its grant is in force now, so what it replaces ends with no moment
	// when neither covers what both do.
	ctx, withdraw := context.WithCancelCause(ctx)
	defer withdraw(nil)
	release := a.registerTicket(tg.Token().Root, g.Token().Root, roleOf(tg.Token()), seq, withdraw)
	defer release()

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
	case heldEnded:
		if errors.Is(context.Cause(ctx), errSuperseded) {
			metricTickets.WithLabelValues(ticketSuperseded).Inc()
			slog.Debug("relay: ticket superseded by a later one for its root", "id", a.req.ID, "root", tg.Token().Root)
			// not actionable: the grant ends either way, and a failure to
			// tell the presenter is its stream failing.
			_ = w.Refuse(moqt.NoError, "superseded by a later ticket for its root")
		}
	}
}

// Outcomes of a ticket, the result label of metricTickets.
const (
	ticketGranted      = "granted"
	ticketRefused      = "refused"
	ticketRevoked      = "revoked"
	ticketExpired      = "expired"
	ticketSuperseded   = "superseded"
	ticketError        = "error"
	ticketNotSupported = "not_supported"
)
