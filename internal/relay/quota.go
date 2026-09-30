package relay

import (
	"log/slog"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/credential"
)

// reasonQuota is the close reason of a session refused by a service quota.
const reasonQuota = "quota"

// withinQuota reports whether key's project may admit one more session in
// the given role on this relay: a live broadcast for a publisher, a session
// for a subscriber. Quotas come from the trust snapshot, so only a managed
// relay enforces them. They are soft: concurrent admissions may overshoot a
// quota by the few that pass the check together.
func (s *Server) withinQuota(key credential.Key, subscriber bool) bool {
	if s.trust == nil {
		return true
	}
	q := s.trust.Quotas(key.ProjectID)
	limit, role := q.Broadcasts, rolePublisher
	if subscriber {
		limit, role = q.SubscriberSessions, roleSubscriber
	}
	if limit == nil || s.admitted.count(key.ProjectID, subscriber) < *limit {
		return true
	}
	metricQuotaRefusals.WithLabelValues(role).Inc()
	slog.Info("relay: refused by service quota", "project_id", key.ProjectID, "role", role, "quota", *limit)
	return false
}

// refuseSubscriberQuota closes a subscriber session whose project is at its
// subscriber-session quota, with reason quota so the client can tell why.
func refuseSubscriberQuota(sess *moqt.Session) {
	metricSessionsEnded.WithLabelValues(reasonQuota).Inc()
	_ = sess.CloseWithError(moqt.UnauthorizedSessionErrorCode, reasonQuota)
}
