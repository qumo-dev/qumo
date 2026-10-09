package funnel

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/okdaichi/qumo-ledger/ingest"
	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/token"
)

// authorizer returns the check a request passes: the bearer credential must be
// one the relay would admit.
//
// A write needs a record scope matching the track's broadcast and name, and
// records as the credential's subject (its sub), or with no sender when it
// names none; a publish scope, which sends through the relay, does not write.
// A read needs a fetch scope matching them. A path_auth credential is read as
// scopes (token.Verify): its sub permits reading at its path and beneath it,
// and its pub, being publish, permits no writing.
//
// The credential is checked on every request, so one that expires or whose
// key leaves the set is refused at once. A nil verifier checks nothing and
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
		grant, err := v.Authorize(r.Context(), auth.Request{
			Event:  auth.EventConnect,
			Remote: r.RemoteAddr,
			Path:   r.URL.Path,
			Query:  url.Values{"jwt": {credential}}.Encode(),
		})
		if err != nil {
			if refused, ok := errors.AsType[auth.RefusedError](err); ok && refused.Status == http.StatusUnauthorized {
				return "", fmt.Errorf("funnel: %w: %w", ingest.ErrUnauthenticated, err)
			}
			return "", fmt.Errorf("funnel: %w", err)
		}
		broadcast, name := moqt.BroadcastPath(t.BroadcastPath), moqt.TrackName(t.TrackName)
		if access == ingest.Read {
			if !grant.Allows(token.ActionFetch, broadcast, name) {
				return "", fmt.Errorf("funnel: the credential may not read %s track %q", broadcast, name)
			}
			return "", nil
		}
		if !grant.Allows(token.ActionRecord, broadcast, name) {
			return "", fmt.Errorf("funnel: the credential may not record into %s track %q", broadcast, name)
		}
		return grant.Subject(), nil
	}
}

// bearer returns the credential of an "Authorization: Bearer" header.
func bearer(r *http.Request) (string, bool) {
	scheme, credential, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || credential == "" {
		return "", false
	}
	return credential, true
}
