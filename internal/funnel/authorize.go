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
)

// authorizer returns the check a contributor's requests pass: the bearer
// credential must be one the relay would admit, and must grant publishing at
// the track's broadcast path. The credential is checked on every request, so
// one that expires or whose key leaves the set stops the contribution. A nil
// verifier checks nothing.
func authorizer(v *auth.Verifier) func(*http.Request, ingest.Track) error {
	if v == nil {
		return nil
	}
	return func(r *http.Request, t ingest.Track) error {
		credential, ok := bearer(r)
		if !ok {
			return fmt.Errorf("funnel: no bearer credential: %w", ingest.ErrUnauthenticated)
		}
		grant, err := v.Authorize(r.Context(), auth.Request{
			Event:  auth.EventConnect,
			Remote: r.RemoteAddr,
			Path:   r.URL.Path,
			Query:  url.Values{"jwt": {credential}}.Encode(),
		})
		if err != nil {
			if refused, ok := errors.AsType[auth.RefusedError](err); ok && refused.Status == http.StatusUnauthorized {
				return fmt.Errorf("funnel: %w: %w", ingest.ErrUnauthenticated, err)
			}
			return fmt.Errorf("funnel: %w", err)
		}
		if path := moqt.BroadcastPath(t.BroadcastPath); !grant.Publish.Contains(path) {
			return fmt.Errorf("funnel: the credential may not publish at %s", path)
		}
		return nil
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
