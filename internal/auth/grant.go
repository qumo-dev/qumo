package auth

import (
	"fmt"
	"strings"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/qumo-dev/qumo/token"
)

// Grant is what a session may do: the actions its scopes permit, from its
// credential or from an Authorize of its own, and the subject that names it.
type Grant struct {
	// scopes are what the session may do; whatever none permits is denied.
	scopes []token.Scope
	// subject is the credential's sub: who the bearer is. Empty names no
	// one.
	subject string
	// expires is when the session must end; zero means never.
	expires time.Time
	// revalidate is how often to check again; zero means never.
	revalidate time.Duration
}

// NewGrant returns a grant for the subtree patterns publish and subscribe
// ("**" for everything, "a/b/**" for a/b and everything beneath it). Each
// becomes a scope on every track of its subtree: a publish pattern permits
// publishing, and a subscribe pattern subscribing and fetching, as a
// path_auth credential's pub and sub do. "**" is a prefix scope with an empty
// broadcast, which reaches every broadcast. The session ends at expires
// (zero: never) and is checked again every revalidate (zero: never). It is
// for an Authorize other than the Verifier's.
func NewGrant(publish, subscribe []string, expires time.Time, revalidate time.Duration) (*Grant, error) {
	g := &Grant{expires: expires, revalidate: revalidate}
	for _, role := range []struct {
		name     string
		patterns []string
		actions  []token.Action
	}{
		{"publish", publish, []token.Action{token.ActionPublish}},
		{"subscribe", subscribe, []token.Action{token.ActionSubscribe, token.ActionFetch}},
	} {
		for _, raw := range role.patterns {
			base, err := parsePattern(strings.TrimSpace(raw))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", role.name, err)
			}
			g.scopes = append(g.scopes, token.Scope{Actions: role.actions, Broadcast: base, Prefix: true})
		}
	}
	return g, nil
}

// Expires returns when the session must end: the credential's expiry. The
// zero Time means the grant does not expire.
func (g *Grant) Expires() time.Time {
	return g.expires
}

// Subject returns who the credential says the bearer is (its sub), or ""
// when it names no one.
func (g *Grant) Subject() string {
	return g.subject
}

// Allows reports whether a scope of the grant permits action on the track
// named track of the broadcast at path.
func (g *Grant) Allows(action token.Action, path moqt.BroadcastPath, track moqt.TrackName) bool {
	for _, s := range g.scopes {
		if s.Allows(action, path.String(), string(track)) {
			return true
		}
	}
	return false
}

// Announces reports whether the session may announce the broadcast at path:
// whether a scope permits publishing every track of it. A broadcast has one
// publisher, so a scope naming one track doesn't announce: it writes that
// track into a broadcast someone else announces, such as a funnel's.
func (g *Grant) Announces(path moqt.BroadcastPath) bool {
	for _, s := range g.scopes {
		if s.Track == "" && s.Allows(token.ActionPublish, path.String(), "") {
			return true
		}
	}
	return false
}

// Revalidate returns how often the relay checks a live session again. Zero
// means never.
func (g *Grant) Revalidate() time.Duration {
	return g.revalidate
}

// parsePattern returns the path a subtree pattern covers, relative to "/":
// "a/b" for "a/b/**" (a/b and everything beneath it, on "/" boundaries), and
// "" for "**" (everything).
func parsePattern(s string) (string, error) {
	if s == "**" {
		return "", nil
	}
	base, ok := strings.CutSuffix(s, "/**")
	if !ok {
		return "", fmt.Errorf("pattern %q: only subtree patterns (\"**\", \"a/b/**\") are supported", s)
	}
	for seg := range strings.SplitSeq(base, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.Contains(seg, "*") {
			return "", fmt.Errorf("pattern %q: bad segment %q", s, seg)
		}
	}
	return base, nil
}
