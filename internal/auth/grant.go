package auth

import (
	"time"

	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/qumo-dev/qumo/token"
)

// Grant is what a session may do: the token grant of its credential, or of
// an Authorize of its own, with how often to check it again.
type Grant struct {
	// grant is the root and patterns the session may act on, and the
	// subject that names it; whatever no pattern matches is denied.
	grant token.Grant
	// revalidate is how often to check again; zero means never.
	revalidate time.Duration
	// keyID is the kid of the key that signed the credential, or "" for a
	// grant an Authorize of its own made.
	keyID string
	// issued is when the credential was issued (its iat), or zero.
	issued time.Time
	// expires is when the credential expires, or zero.
	expires time.Time
	// anonymous is a session that connected with no credential.
	anonymous bool
}

// NewGrant returns a grant of g, checked again every revalidate (zero:
// never). It is for an Authorize other than the Verifier's.
func NewGrant(g token.Grant, revalidate time.Duration) *Grant {
	return &Grant{grant: g, revalidate: revalidate}
}

// Anonymous reports whether the session connected with no credential. It
// may do nothing until a ticket presented on its Auth Stream grants it
// something, and every ticket it holds is signed by the key of the first one
// granted: that key's tenant is the session's.
func (g *Grant) Anonymous() bool {
	return g.anonymous
}

// Subject returns who the credential says the bearer is (its sub), or ""
// when it names no one.
func (g *Grant) Subject() string {
	return g.grant.Subject
}

// KeyID returns the kid of the key that signed the credential, or "" for a
// grant an Authorize other than the Verifier's made.
func (g *Grant) KeyID() string {
	return g.keyID
}

// IssuedAt returns when the credential was issued (its iat), or the zero
// time when it doesn't say.
func (g *Grant) IssuedAt() time.Time {
	return g.issued
}

// Expires returns when the credential expires, or the zero time when it
// doesn't say.
func (g *Grant) Expires() time.Time {
	return g.expires
}

// Token returns the token grant: the MoQ grant a relay applies to a session
// (its embedded moqt.Grant), and the actions a funnel reads.
func (g *Grant) Token() token.Grant {
	return g.grant
}

// Allows reports whether a pattern of the grant for action matches the
// broadcast at path.
func (g *Grant) Allows(action token.Action, path moqt.BroadcastPath) bool {
	return g.grant.Allows(action, path.String())
}

// Revalidate returns how often the relay checks a live session again. Zero
// means never.
func (g *Grant) Revalidate() time.Duration {
	return g.revalidate
}
