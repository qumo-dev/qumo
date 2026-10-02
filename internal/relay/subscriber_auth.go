package relay

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/credential"
)

const (
	// reservedPathPrefix is the broadcast-path prefix of relay control
	// announcements. Announcements under it are never routed.
	reservedPathPrefix = "/.qumo/"
	// sessionAuthPath is where a client announces its session credential: a
	// broadcast with one "auth" track, carrying the credential its
	// subscriptions are authorized by (ADR 0035; see #418).
	sessionAuthPath = moqt.BroadcastPath("/.qumo/session")
	// credentialWait is how long a credential may take to arrive: the first
	// group on an auth track, and, for a SUBSCRIBE sent before the session
	// credential was accepted, the wait for it.
	credentialWait = 5 * time.Second
)

// subscriberGate holds an untrusted session's subscription authorization:
// its session credential, once accepted.
type subscriberGate struct {
	ready chan struct{} // closed when the first session credential is accepted
	once  sync.Once
	ad    atomic.Pointer[admission]
	// reading is held while a session credential is being read, so a second
	// announcement cannot admit a second credential alongside it.
	reading atomic.Bool
}

func newSubscriberGate() *subscriberGate {
	return &subscriberGate{ready: make(chan struct{})}
}

// accept records the session's first accepted credential.
func (g *subscriberGate) accept(ad *admission) {
	g.once.Do(func() {
		g.ad.Store(ad)
		close(g.ready)
	})
}

// connStateKey is the context key under which the relay's ConnContext hook
// stores each connection's *connState, the way a net/http server keeps
// per-connection state for its handlers.
type connStateKey struct{}

// connState is what the relay keeps per connection (one MoQ session each).
type connState struct {
	// gate authorizes the connection's subscriptions. It is nil for a
	// connection the relay does not gate: a trusted relay peer, or any
	// connection when credential auth is off.
	gate *subscriberGate
	// session records why the relay ended the session, shared by all of its
	// admissions.
	session sessionState
}

// newConnState decides, when a connection is accepted, whether its
// subscriptions need a session credential: with credential auth on, every
// WebTransport connection, and every native-QUIC one that is not a trusted
// peer (mTLS or PEER_CIDRS).
func (s *Server) newConnState(conn moqt.StreamConn) *connState {
	if s.verifier == nil {
		return &connState{}
	}
	state := conn.TLS()
	if state != nil && state.NegotiatedProtocol == moqt.NextProtoMOQ {
		var cidrs []netip.Prefix
		if s.Config != nil {
			cidrs = s.Config.PeerCIDRs
		}
		if isTrustedPeer(state, conn.RemoteAddr(), cidrs) {
			return &connState{}
		}
	}
	return &connState{gate: newSubscriberGate()}
}

// peerDialContext returns the context to dial a configured peer with. The
// dialed session is a trusted relay peer that subscribes back over it, but it
// does not pass through the ConnContext hook, which only sees accepted
// connections. A client connection's context keeps the dial context's values,
// so this gives the session, and the SUBSCRIBEs it carries, a state of its own
// that is not gated.
func peerDialContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, connStateKey{}, &connState{})
}

// connStateFrom returns the connection state in ctx: a session's context or
// a TrackWriter's, both of which carry the ConnContext values.
func connStateFrom(ctx context.Context) (*connState, bool) {
	cs, ok := ctx.Value(connStateKey{}).(*connState)
	return cs, ok
}

// isReservedPath reports whether an announcement is relay control, never
// routed.
func isReservedPath(path moqt.BroadcastPath) bool {
	return strings.HasPrefix(string(path), reservedPathPrefix)
}

// handleSessionCredential reads and verifies an untrusted session's session
// credential from ann, then tracks it for refresh and expiry. A session that
// never presents one has every SUBSCRIBE refused.
func (s *Server) handleSessionCredential(sess *moqt.Session, ann *moqt.Announcement) {
	cs, ok := connStateFrom(sess.Context())
	if !ok || cs.gate == nil || ann.BroadcastPath() != sessionAuthPath {
		return
	}
	g := cs.gate
	if g.ad.Load() != nil || !g.reading.CompareAndSwap(false, true) {
		slog.Warn("relay: ignoring a second session credential announcement", "remote", sess.RemoteAddr())
		return
	}
	cred, reader, err := s.readAuthTrack(sess.Context(), sess, ann, s.verifier.Verify)
	if err != nil {
		// Released so the client can present a credential again.
		g.reading.Store(false)
		slog.Warn("relay: session credential rejected", "remote", sess.RemoteAddr(), "error", err)
		return
	}
	if !s.withinQuota(cred.Key, true) {
		_ = reader.reader.Close()
		refuseSubscriberQuota(sess)
		return
	}
	g.accept(s.admitSubscriber(sess, ann, cred, reader))
}

// authorizeSubscribe admits a SUBSCRIBE served by a relayHandler. Connections
// the relay does not gate (trusted peers) are admitted. A gated session's
// SUBSCRIBE waits up to credentialWait for its session credential, and is
// admitted only if that credential's subscribe grant covers the path.
func (s *Server) authorizeSubscribe(tw *moqt.TrackWriter) bool {
	cs, ok := connStateFrom(tw.Context())
	if !ok {
		// Every connection the relay accepts passes through its ConnContext
		// hook, and every peer it dials carries peerDialContext's state; a
		// SUBSCRIBE without that state is refused, not trusted.
		metricSubscribeAuthz.WithLabelValues("unidentified").Inc()
		return false
	}
	if cs.gate == nil {
		return true
	}
	g := cs.gate
	select {
	case <-g.ready:
	case <-tw.Context().Done():
		return false
	case <-time.After(credentialWait):
		metricSubscribeAuthz.WithLabelValues("no_credential").Inc()
		return false
	}
	if !g.ad.Load().subscribe(string(tw.BroadcastPath), tw.Context()) {
		metricSubscribeAuthz.WithLabelValues("not_covered").Inc()
		return false
	}
	metricSubscribeAuthz.WithLabelValues("admitted").Inc()
	return true
}

// verifyFunc checks one credential token.
type verifyFunc func(ctx context.Context, token string) (credential.Credential, error)

// readAuthTrack subscribes to the "auth" track on ann, reads the credential
// from its first group within credentialWait, and checks it with verify. On
// success it returns the credential and the still-open track, on which the
// client sends refreshed credentials.
func (s *Server) readAuthTrack(ctx context.Context, sess *moqt.Session, ann *moqt.Announcement, verify verifyFunc) (credential.Credential, *authTrack, error) {
	authCtx, cancel := context.WithTimeout(ctx, credentialWait)
	defer cancel()

	// The context only bounds opening the subscription; the returned reader
	// stays open until closed.
	reader, err := sess.Subscribe(authCtx, ann.BroadcastPath(), authTrackName, nil)
	if err != nil {
		return credential.Credential{}, nil, fmt.Errorf("subscribe auth track: %w", err)
	}
	gr, err := reader.AcceptGroup(authCtx)
	if err != nil {
		_ = reader.Close()
		return credential.Credential{}, nil, fmt.Errorf("accept auth group: %w", err)
	}
	buf := s.framePool.Get()
	defer s.framePool.Put(buf)
	token, err := readCredential(gr, buf)
	if err != nil {
		_ = reader.Close()
		return credential.Credential{}, nil, err
	}
	cred, err := verify(authCtx, token)
	if err != nil {
		_ = reader.Close()
		return credential.Credential{}, nil, fmt.Errorf("verify credential: %w", err)
	}
	return cred, &authTrack{reader: reader, last: gr.GroupSequence()}, nil
}
