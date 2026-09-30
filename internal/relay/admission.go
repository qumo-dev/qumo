package relay

import (
	"context"
	"log/slog"
	"sync"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/credential"
)

// admission is a live session admitted under a credential. On a managed relay
// the trust snapshot can end it: its key revoked or removed, or its project
// suspended.
type admission struct {
	sess *moqt.Session
	key  credential.Key
}

// admissions is the set of live admissions the trust snapshot is enforced on.
// The zero value is ready to use.
type admissions struct {
	mu  sync.Mutex
	set map[*admission]struct{}
}

// add registers ad until the session or the announcement ends, whichever is
// first.
func (a *admissions) add(ad *admission, ann *moqt.Announcement) {
	a.mu.Lock()
	if a.set == nil {
		a.set = make(map[*admission]struct{})
	}
	a.set[ad] = struct{}{}
	a.mu.Unlock()

	remove := func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		delete(a.set, ad)
	}
	context.AfterFunc(ad.sess.Context(), remove)
	ann.AfterFunc(remove)
}

// list returns the current admissions.
func (a *admissions) list() []*admission {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*admission, 0, len(a.set))
	for ad := range a.set {
		out = append(out, ad)
	}
	return out
}

// enforceTrust ends every admitted session the current trust snapshot no
// longer allows. It runs after each snapshot swap, so a revocation or
// suspension takes effect within one poll.
func (s *Server) enforceTrust() {
	ended := make(map[*moqt.Session]struct{})
	for _, ad := range s.admitted.list() {
		if _, done := ended[ad.sess]; done {
			continue
		}
		reason, ok := s.trust.Live(ad.key.ID, ad.key.ProjectID)
		if ok {
			continue
		}
		ended[ad.sess] = struct{}{}
		endSession(ad.sess, reason)
		slog.Info("relay: ended session by trust snapshot",
			"reason", reason, "kid", ad.key.ID, "project_id", ad.key.ProjectID, "remote", ad.sess.RemoteAddr())
	}
}

// endSession closes sess with the Unauthorized code and reason as the phrase,
// which clients read to tell why (key_revoked, project_suspended).
func endSession(sess *moqt.Session, reason string) {
	metricSessionsEnded.WithLabelValues(reason).Inc()
	_ = sess.CloseWithError(moqt.UnauthorizedSessionErrorCode, reason)
}
