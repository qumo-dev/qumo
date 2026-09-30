package relay

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/credential"
)

const (
	// defaultExpiryLeeway is how long past its credential's exp a session is
	// kept, absorbing clock skew between the app and the relay (ADR 0035).
	defaultExpiryLeeway = 60 * time.Second
	// defaultMinRefreshInterval bounds how often a session's refreshes are
	// verified, so a client cannot make the relay verify in a loop.
	defaultMinRefreshInterval = 10 * time.Second
	// maxCredentialBytes bounds one credential on the auth track.
	maxCredentialBytes = 8 << 10
)

// Reasons the relay ends a session or refuses a refresh.
const (
	reasonExpired = "credential_expired"

	refreshAccepted      = "accepted"
	refreshChecks        = "checks"
	refreshOtherProject  = "other_project"
	refreshWidens        = "widens"
	refreshUncoveredPath = "uncovered_path"
	refreshRate          = "rate"
	refreshSize          = "size"
	refreshStale         = "stale"
)

var errCredentialTooLarge = errors.New("auth track: credential exceeds the size limit")

// authTrack is a client's open auth track and the sequence of the newest
// credential group read from it. Each group travels on its own stream, so
// groups can arrive out of order; one older than the newest seen is stale.
type authTrack struct {
	reader *moqt.TrackReader
	last   moqt.GroupSequence
}

// admission is a live session admitted under a credential: a publisher's
// announcement, or a subscriber's session credential. It ends at its current
// credential's expiry unless a refresh on the auth track moves that; on a
// managed relay the trust snapshot can end it sooner.
type admission struct {
	sess *moqt.Session
	// path is the announced broadcast path for a publisher, and the session
	// auth path for a subscriber.
	path       string
	subscriber bool

	mu       sync.Mutex
	cred     credential.Credential
	deadline time.Time
	timer    *time.Timer
	// open counts a subscriber's open subscriptions by broadcast path. It
	// shares mu with cred, so a narrowing refresh and a new SUBSCRIBE cannot
	// pass each other.
	open map[string]int
}

// coversLocked reports whether c covers everything the session is live on:
// a publisher's broadcast, or each of a subscriber's open subscriptions.
// Caller holds ad.mu.
func (ad *admission) coversLocked(c credential.Credential) bool {
	if !ad.subscriber {
		return c.CoversPublish(ad.path)
	}
	for path := range ad.open {
		if !c.CoversSubscribe(path) {
			return false
		}
	}
	return true
}

// subscribe admits a subscription to path if the current credential covers
// it, and counts it open until done.
func (ad *admission) subscribe(path string, done context.Context) bool {
	ad.mu.Lock()
	defer ad.mu.Unlock()
	if !ad.cred.CoversSubscribe(path) {
		return false
	}
	ad.open[path]++
	context.AfterFunc(done, func() {
		ad.mu.Lock()
		defer ad.mu.Unlock()
		if ad.open[path]--; ad.open[path] <= 0 {
			delete(ad.open, path)
		}
	})
	return true
}

// key returns the key of the session's current credential.
func (ad *admission) key() credential.Key {
	ad.mu.Lock()
	defer ad.mu.Unlock()
	return ad.cred.Key
}

// admissions is the set of live admissions. The zero value is ready to use.
type admissions struct {
	mu  sync.Mutex
	set map[*admission]struct{}
}

func (a *admissions) add(ad *admission) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.set == nil {
		a.set = make(map[*admission]struct{})
	}
	a.set[ad] = struct{}{}
}

func (a *admissions) remove(ad *admission) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.set, ad)
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

// admit registers a publisher admitted under cred, arms its expiry, and
// reads refreshed credentials from reader, the announcement's auth track,
// until the announcement or the session ends.
func (s *Server) admit(sess *moqt.Session, ann *moqt.Announcement, cred credential.Credential, auth *authTrack) {
	s.track(&admission{sess: sess, path: ann.BroadcastPath().String(), cred: cred}, ann, auth)
}

// admitSubscriber registers a session credential for sess's subscriptions,
// read from ann, the session auth announcement.
func (s *Server) admitSubscriber(sess *moqt.Session, ann *moqt.Announcement, cred credential.Credential, auth *authTrack) *admission {
	ad := &admission{sess: sess, path: ann.BroadcastPath().String(), subscriber: true, cred: cred, open: make(map[string]int)}
	s.track(ad, ann, auth)
	return ad
}

// track arms ad's expiry, registers it for trust enforcement, and reads
// refreshed credentials from reader until ann or the session ends.
func (s *Server) track(ad *admission, ann *moqt.Announcement, auth *authTrack) {
	sess, cred := ad.sess, ad.cred
	ad.deadline = cred.ExpiresAt.Add(s.expiryLeeway())
	ad.mu.Lock()
	ad.timer = time.AfterFunc(time.Until(ad.deadline), func() { s.expire(ad) })
	ad.mu.Unlock()
	s.admitted.add(ad)

	ctx, cancel := context.WithCancel(sess.Context())
	ann.AfterFunc(cancel)
	context.AfterFunc(ctx, func() {
		s.admitted.remove(ad)
		ad.mu.Lock()
		ad.timer.Stop()
		ad.mu.Unlock()
	})
	go s.readRefreshes(ctx, ad, auth)
}

// expire ends the session once its deadline has passed. A refresh may have
// moved the deadline since the timer was armed; then it re-arms instead.
func (s *Server) expire(ad *admission) {
	ad.mu.Lock()
	if remaining := time.Until(ad.deadline); remaining > 0 {
		ad.timer.Reset(remaining)
		ad.mu.Unlock()
		return
	}
	kid := ad.cred.Key.ID
	ad.mu.Unlock()
	endSession(ad.sess, reasonExpired)
	slog.Info("relay: ended session at credential expiry", "kid", kid, "broadcast_path", ad.path, "remote", ad.sess.RemoteAddr())
}

// readRefreshes verifies each later group on the auth track as a refreshed
// credential, newest only and at most once per minimum refresh interval,
// until ctx ends. It owns the reader and closes it on return: gomoqt's
// TrackReader.Close is not safe to call while another goroutine is in
// AcceptGroup (qumo-dev/gomoqt#432).
func (s *Server) readRefreshes(ctx context.Context, ad *admission, auth *authTrack) {
	defer func() { _ = auth.reader.Close() }()
	buf := s.framePool.Get()
	defer s.framePool.Put(buf)
	var last time.Time
	for {
		gr, err := auth.reader.AcceptGroup(ctx)
		if err != nil {
			return
		}
		seq := gr.GroupSequence()
		if seq <= auth.last {
			gr.CancelRead(moqt.InternalGroupErrorCode)
			s.refreshed(ad, refreshStale, nil)
			continue
		}
		auth.last = seq
		token, err := readCredential(gr, buf)
		switch {
		case errors.Is(err, errCredentialTooLarge):
			s.refreshed(ad, refreshSize, nil)
			continue
		case err != nil:
			s.refreshed(ad, refreshChecks, err)
			continue
		}
		if !last.IsZero() && time.Since(last) < s.minRefreshInterval() {
			s.refreshed(ad, refreshRate, nil)
			continue
		}
		last = time.Now()
		s.refresh(ctx, ad, token)
	}
}

// refresh replaces ad's credential with token if it is a valid refresh:
// it passes every check, its key is active and of the same project (the kid
// may change), it grants nothing the current credential does not, and it
// still covers everything the session is live on. Otherwise the credential and
// deadline stay.
func (s *Server) refresh(ctx context.Context, ad *admission, token string) {
	next, err := s.verifier.Verify(ctx, token)
	if err != nil {
		s.refreshed(ad, refreshChecks, err)
		return
	}
	ad.mu.Lock()
	defer ad.mu.Unlock()
	switch {
	case next.Key.ProjectID != ad.cred.Key.ProjectID:
		s.refreshedLocked(ad, refreshOtherProject, nil)
	case !next.Within(ad.cred):
		s.refreshedLocked(ad, refreshWidens, nil)
	case !ad.coversLocked(next):
		s.refreshedLocked(ad, refreshUncoveredPath, nil)
	default:
		ad.cred = next
		ad.deadline = next.ExpiresAt.Add(s.expiryLeeway())
		ad.timer.Reset(time.Until(ad.deadline))
		s.refreshedLocked(ad, refreshAccepted, nil)
	}
}

func (s *Server) refreshed(ad *admission, result string, err error) {
	ad.mu.Lock()
	defer ad.mu.Unlock()
	s.refreshedLocked(ad, result, err)
}

// refreshedLocked records a refresh outcome. Caller holds ad.mu.
func (s *Server) refreshedLocked(ad *admission, result string, err error) {
	metricRefreshes.WithLabelValues(result).Inc()
	if result == refreshAccepted {
		slog.Debug("relay: credential refreshed", "kid", ad.cred.Key.ID, "broadcast_path", ad.path, "expires_at", ad.cred.ExpiresAt)
		return
	}
	slog.Info("relay: credential refresh refused", "reason", result, "error", err,
		"kid", ad.cred.Key.ID, "broadcast_path", ad.path, "remote", ad.sess.RemoteAddr())
}

// readCredential reads one credential group, refusing one larger than
// maxCredentialBytes.
func readCredential(gr *moqt.GroupReader, buf *moqt.Frame) (string, error) {
	var token []byte
	for frame := range gr.Frames(buf) {
		token = append(token, frame.Body()...)
		if len(token) > maxCredentialBytes {
			gr.CancelRead(moqt.InternalGroupErrorCode)
			return "", errCredentialTooLarge
		}
	}
	if len(token) == 0 {
		return "", errors.New("auth track: empty JWT")
	}
	return string(token), nil
}

// enforceTrust ends every admitted session the current trust snapshot no
// longer allows. It runs after each snapshot swap, so a revocation or
// suspension takes effect within one poll. Revocation applies to the key of
// a session's current credential.
func (s *Server) enforceTrust() {
	ended := make(map[*moqt.Session]struct{})
	for _, ad := range s.admitted.list() {
		if _, done := ended[ad.sess]; done {
			continue
		}
		key := ad.key()
		reason, ok := s.trust.Live(key.ID, key.ProjectID)
		if ok {
			continue
		}
		ended[ad.sess] = struct{}{}
		endSession(ad.sess, reason)
		slog.Info("relay: ended session by trust snapshot",
			"reason", reason, "kid", key.ID, "project_id", key.ProjectID, "remote", ad.sess.RemoteAddr())
	}
}

// endSession closes sess with the Unauthorized code and reason as the phrase,
// which clients read to tell why (credential_expired, key_revoked,
// project_suspended).
func endSession(sess *moqt.Session, reason string) {
	metricSessionsEnded.WithLabelValues(reason).Inc()
	_ = sess.CloseWithError(moqt.UnauthorizedSessionErrorCode, reason)
}

func (s *Server) expiryLeeway() time.Duration {
	if s.credentialExpiryLeeway > 0 {
		return s.credentialExpiryLeeway
	}
	return defaultExpiryLeeway
}

func (s *Server) minRefreshInterval() time.Duration {
	if s.credentialRefreshInterval > 0 {
		return s.credentialRefreshInterval
	}
	return defaultMinRefreshInterval
}
