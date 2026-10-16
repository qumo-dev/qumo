package funnel

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/okdaichi/qumo-ledger/ingest"
	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/token"
)

// authorizer returns the check a request passes: the bearer credential must be
// one the relay would admit, with a pattern for the access's action that
// matches the track's broadcast.
//
// Creating a track needs create; it is for the app, which decides which tracks
// a broadcast has. A write needs post, and records into a track that exists,
// as the credential's subject (its sub), or with no sender when it names
// none. A read needs fetch, and a redaction redact, carrying the credential's
// subject as its sender. Publish, which sends through a relay, permits
// nothing here. A path_auth credential is read as the grant it amounts to
// (token.Verify): its sub permits reading at its path and beneath it.
//
// The credential is checked on every request, so one that expires or whose
// key leaves the set is refused at once. A key paused for publishing refuses
// creating only, even a track that exists. A nil verifier checks nothing and
// names no sender.
func authorizer(v *auth.Verifier) func(*http.Request, ingest.Track, ingest.Access) (string, error) {
	if v == nil {
		return nil
	}
	return func(r *http.Request, t ingest.Track, access ingest.Access) (string, error) {
		credential, ok := bearer(r)
		if !ok {
			return "", fmt.Errorf("funnel: no bearer credential: %w", ingest.ErrUnauthenticated)
		}
		action, sender, ok := accessAction(access)
		if !ok {
			// An access added later is refused until an action grants it, so no
			// credential gains it by default.
			return "", fmt.Errorf("funnel: no credential grants access %d to %s track %q", access, t.BroadcastPath, t.TrackName)
		}
		grant, err := v.Check(credential, action, moqt.BroadcastPath(t.BroadcastPath))
		if err != nil {
			if refused, ok := errors.AsType[auth.RefusedError](err); ok && refused.Status == http.StatusUnauthorized {
				return "", fmt.Errorf("funnel: %w: %w", ingest.ErrUnauthenticated, err)
			}
			return "", fmt.Errorf("funnel: %w", err)
		}
		if !grant.Allows(action, moqt.BroadcastPath(t.BroadcastPath)) {
			return "", fmt.Errorf("funnel: the credential may not %s %s track %q", action, t.BroadcastPath, t.TrackName)
		}
		if !sender {
			return "", nil
		}
		return grant.Subject(), nil
	}
}

// accessAction returns the action a pattern must grant for access, and
// whether the credential's subject is the sender of what it makes.
func accessAction(access ingest.Access) (action token.Action, sender, ok bool) {
	switch access {
	case ingest.Create:
		return token.ActionCreate, false, true
	case ingest.Write:
		return token.ActionPost, true, true
	case ingest.Read:
		return token.ActionFetch, false, true
	case ingest.Redact:
		return token.ActionRedact, true, true
	}
	return "", false, false
}

// bearer returns the credential of an "Authorization: Bearer" header.
func bearer(r *http.Request) (string, bool) {
	scheme, credential, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || credential == "" {
		return "", false
	}
	return credential, true
}
