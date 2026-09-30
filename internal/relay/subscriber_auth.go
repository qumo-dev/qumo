package relay

import (
	"context"
	"fmt"
	"log/slog"
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

// gateSession starts subscription authorization for an untrusted session and
// returns a function that ends it with the session.
func (s *Server) gateSession(sess *moqt.Session) func() {
	s.gates.Store(sess, newSubscriberGate())
	return func() { s.gates.Delete(sess) }
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
	v, ok := s.gates.Load(sess)
	if !ok || ann.BroadcastPath() != sessionAuthPath {
		return
	}
	g := v.(*subscriberGate)
	if g.ad.Load() != nil {
		slog.Warn("relay: ignoring a second session credential announcement", "remote", sess.RemoteAddr())
		return
	}
	cred, reader, err := s.readAuthTrack(sess.Context(), sess, ann, s.verifier.Verify)
	if err != nil {
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

// authorizeSubscribe admits a SUBSCRIBE served by a relayHandler. Sessions
// the relay does not gate (trusted peers) are admitted. A gated session's
// SUBSCRIBE waits up to credentialWait for its session credential, and is
// admitted only if that credential's subscribe grant covers the path.
func (s *Server) authorizeSubscribe(tw *moqt.TrackWriter) bool {
	sess, ok := moqt.SessionFromContext(tw.Context())
	if !ok {
		metricSubscribeAuthz.WithLabelValues("no_session").Inc()
		return false
	}
	v, gated := s.gates.Load(sess)
	if !gated {
		return true
	}
	g := v.(*subscriberGate)
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
