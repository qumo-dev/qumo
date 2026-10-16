package auth

import (
	"slices"
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
	// revalidate is how often to check again; zero means never.
	revalidate time.Duration
	// keyID is the kid of the key that signed the credential, or "" for a
	// grant an Authorize of its own made.
	keyID string
	// expires is when the credential expires, or zero.
	expires time.Time
	// keyPrefix is the prefix of the key that signed the credential: what
	// it grants lies at or beneath it.
	keyPrefix string
}

// NewGrant returns a grant of scopes, checked again every revalidate (zero:
// never). It is for an Authorize other than the Verifier's.
func NewGrant(scopes []token.Scope, revalidate time.Duration) *Grant {
	return &Grant{scopes: scopes, revalidate: revalidate}
}

// Subject returns who the credential says the bearer is (its sub), or ""
// when it names no one.
func (g *Grant) Subject() string {
	return g.subject
}

// KeyID returns the kid of the key that signed the credential, or "" for a
// grant an Authorize other than the Verifier's made.
func (g *Grant) KeyID() string {
	return g.keyID
}

// Expires returns when the credential expires, or the zero time when it
// doesn't say.
func (g *Grant) Expires() time.Time {
	return g.expires
}

// MoQ returns what the grant lets a relay session do, as a MoQ grant: its
// publish and subscribe scopes, under the deepest directory they share as the
// root, which bounds the announcements the session may list. A prefix scope
// counts as its directory, an exact one as the directory it lies in, and the
// scopes become patterns relative to the root: "/x/" for a prefix, "/x" for
// an exact path. The root never lies above the signing key's prefix, so a
// session lists nothing of another tenant: an exact scope on the key's
// prefix itself, whose directory would, is left out. Every other action is
// not the relay's, and a grant with no publish or subscribe scope grants
// nothing (an empty root).
func (g *Grant) MoQ() moqt.Grant {
	type pattern struct {
		path    string // absolute: "/a/b" exact, "/a/b/" beneath
		publish bool
	}
	var patterns []pattern
	root, floor := "", "/"
	if g.keyPrefix != "" {
		floor = "/" + g.keyPrefix + "/"
	}
	for _, s := range g.scopes {
		publish := slices.Contains(s.Actions, token.ActionPublish)
		subscribe := slices.Contains(s.Actions, token.ActionSubscribe)
		if (!publish && !subscribe) || (s.Broadcast == "" && !s.Prefix) {
			continue // not the relay's, or reaching no path
		}
		path, dir := "/"+s.Broadcast, ""
		if s.Prefix {
			path = strings.TrimSuffix(path, "/") + "/"
			dir = path
		} else {
			dir = path[:strings.LastIndex(path, "/")+1]
		}
		if !strings.HasPrefix(dir, floor) {
			continue
		}
		if root == "" {
			root = dir
		} else {
			root = commonDir(root, dir)
		}
		if publish {
			patterns = append(patterns, pattern{path: path, publish: true})
		}
		if subscribe {
			patterns = append(patterns, pattern{path: path})
		}
	}
	if root == "" {
		return moqt.Grant{}
	}
	m := moqt.Grant{Root: root}
	for _, p := range patterns {
		rel := p.path[len(root)-1:]
		if p.publish && !slices.Contains(m.Publish, rel) {
			m.Publish = append(m.Publish, rel)
		}
		if !p.publish && !slices.Contains(m.Subscribe, rel) {
			m.Subscribe = append(m.Subscribe, rel)
		}
	}
	return m
}

// commonDir returns the deepest directory, ending in "/", that both a and b,
// themselves directories, lie in.
func commonDir(a, b string) string {
	for !strings.HasPrefix(b, a) {
		a = a[:strings.LastIndex(strings.TrimSuffix(a, "/"), "/")+1]
	}
	return a
}

// Allows reports whether a scope of the grant permits action on the
// broadcast at path.
func (g *Grant) Allows(action token.Action, path moqt.BroadcastPath) bool {
	for _, s := range g.scopes {
		if s.Allows(action, path.String()) {
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
