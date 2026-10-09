package auth

import (
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
