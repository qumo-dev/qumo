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
// A credential with scopes names the exact tracks it reaches. A write needs a
// publish scope matching the track's broadcast and name, and records as the
// credential's subject (its sub), or with no sender when it names none. A
// read needs a fetch scope matching them.
//
// A credential with path_auth grants paths. A read needs it to grant
// subscribing at the track's broadcast path. A write needs it to grant
// publishing either at the broadcast path, which records with no sender, or
// at one segment beneath it, which names the sender: a credential for
// /room/123/comments/user-42 records into the broadcast /room/123/comments as
// "user-42".
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
		if grant.Allows(token.ActionPublish, broadcast, name) {
			return grant.Subject(), nil
		}
		// A scoped grant has no publish paths, so a sender is never inferred
		// from one.
		if sender, ok := senderOf(grant.Publish.Bases(), t.BroadcastPath); ok {
			return sender, nil
		}
		return "", fmt.Errorf("funnel: the credential may not record into %s track %q", broadcast, name)
	}
}

// senderOf returns the sender a publish grant names for a broadcast: the one
// segment a granted path adds beneath it.
func senderOf(bases []string, broadcastPath string) (string, bool) {
	broadcast := strings.Trim(broadcastPath, "/")
	for _, base := range bases {
		i := strings.LastIndexByte(base, '/')
		if i > 0 && base[:i] == broadcast && base[i+1:] != "" {
			return base[i+1:], true
		}
	}
	return "", false
}

// bearer returns the credential of an "Authorization: Bearer" header.
func bearer(r *http.Request) (string, bool) {
	scheme, credential, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || credential == "" {
		return "", false
	}
	return credential, true
}
