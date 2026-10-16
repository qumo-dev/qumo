package token

import (
	"fmt"
	"strings"

	"github.com/qumo-dev/gomoqt/moqt"
)

// Action is something a grant may let its bearer do on a broadcast path.
type Action string

// The actions a grant names patterns for.
const (
	// ActionPublish is sending a broadcast's tracks through a relay.
	ActionPublish Action = "publish"
	// ActionSubscribe is receiving a track, live at a relay, and reading its
	// history, as a relay's FETCH or a funnel's GET does.
	ActionSubscribe Action = "subscribe"
	// ActionPost is sending one message into a track at a funnel, which
	// gathers many senders' messages into that one track and publishes it.
	// The track must exist: posting never creates one (ActionCreate). A relay
	// never reads it as publishing, so a credential that posts can't send
	// into the relay directly.
	ActionPost Action = "post"
	// ActionRedact is taking a record out of a track at a funnel: the funnel
	// publishes a redaction naming it and deletes its payload. Who may redact
	// which record is the app's to decide, so it belongs in credentials the
	// app keeps to itself, not in a sender's.
	ActionRedact Action = "redact"
	// ActionCreate is creating a track of a broadcast at a funnel, which the
	// funnel then publishes. It is for the app, which decides which tracks a
	// broadcast has; senders post only into the tracks it made. A relay
	// ignores it.
	ActionCreate Action = "create"
)

// Grant is what a token allows its bearer: a MoQ grant (moqt.Grant), which a
// relay applies to the bearer's session as it is, and the actions only a
// funnel reads. Its Root is the directory of broadcast paths it applies to,
// in MoQ's form ("/acme/room/42/"), and each action's patterns are read as
// moqt.Grant reads them, relative to the root: with Root "/acme/room/42/",
// "/7" is the broadcast "/acme/room/42/7" alone, "/7/" every broadcast
// beneath it, "/" every broadcast beneath the root, and a segment may be a
// wildcard ("/{user}/audio"). Whatever no pattern matches is denied.
//
// In a token, Root lies at or beneath the signing key's prefix, so every
// pattern does too. A grant with no root and no patterns grants nothing, as
// moqt.Grant{} does: it names its key's tenant alone. MaxAge is never part of
// a token, whose expiry is its exp; a relay sets it from that.
type Grant struct {
	moqt.Grant
	// Create matches the broadcasts the bearer may create tracks of at a
	// funnel.
	Create []string
	// Post matches the broadcasts the bearer may post into at a funnel.
	Post []string
	// Redact matches the broadcasts the bearer may redact records of at a
	// funnel.
	Redact []string
	// Subject is the bearer (the sub claim); empty names no one.
	Subject string
}

// Patterns returns the patterns that grant action, or nil for an action the
// grant doesn't know.
func (g Grant) Patterns(action Action) []string {
	switch action {
	case ActionPublish:
		return g.Publish
	case ActionSubscribe:
		return g.Subscribe
	case ActionCreate:
		return g.Create
	case ActionPost:
		return g.Post
	case ActionRedact:
		return g.Redact
	}
	return nil
}

// Actions returns every action, in the order a grant lists them.
func Actions() []Action {
	return []Action{ActionPublish, ActionSubscribe, ActionCreate, ActionPost, ActionRedact}
}

// Allows reports whether a pattern of the grant for action matches the
// broadcast at path, such as "/acme/room/42/7"; the leading "/" is optional.
func (g Grant) Allows(action Action, path string) bool {
	// MoQ compares broadcast paths as written, so a path is matched only in
	// its one canonical spelling, relative to "/". Another spelling (a
	// doubled or trailing slash, a "." or ".." segment) names a different
	// broadcast and matches no pattern.
	p, err := normalizePath(path)
	if err != nil || p != strings.TrimPrefix(path, "/") {
		return false
	}
	bp := moqt.BroadcastPath("/" + p)
	for _, s := range g.Patterns(action) {
		pattern, err := moqt.ParseGrantPattern(g.Root, s)
		if err == nil && pattern.Match(bp) {
			return true
		}
	}
	return false
}

// checked returns g if a token may carry it under a key confined to prefix:
// a MoQ grant moqt.Grant.Validate accepts, with no MaxAge and its root in
// Directory's form (ErrInvalid); every other action's patterns parsed as
// moqt.ParseGrantPattern reads them (ErrInvalid); no "." or ".." segment in
// any pattern (ErrInvalid); and a root at or beneath prefix (ErrForbidden).
func (g Grant) checked(prefix string) (Grant, error) {
	if g.MaxAge != 0 {
		return Grant{}, invalid("max age %s: a token's expiry is its exp", g.MaxAge)
	}
	if g.Root == "" {
		for _, a := range Actions() {
			if len(g.Patterns(a)) > 0 {
				return Grant{}, invalid("%s patterns without a root", a)
			}
		}
		// It grants nothing, so it lies within every prefix.
		return g, nil
	}
	if root, err := Directory(g.Root); err != nil || root != g.Root {
		return Grant{}, invalid("root %q is not a directory such as %q", g.Root, "/acme/room/")
	}
	for _, a := range Actions() {
		for _, s := range g.Patterns(a) {
			if err := checkPattern(g.Root, s); err != nil {
				return Grant{}, invalid("%s pattern %q: %v", a, s, err)
			}
		}
	}
	// What is left to refuse is a grant too large for a relay to send.
	if err := g.Validate(); err != nil {
		return Grant{}, invalid("%v", err)
	}
	if !within(g.Root, prefix) {
		return Grant{}, forbidden("root %q lies outside the signing key's prefix %q", g.Root, prefix)
	}
	return g, nil
}

// checkPattern checks s as a pattern relative to root. A "." or ".."
// segment is refused: MoQ would read it literally, while a path elsewhere,
// such as a URL, might resolve it out of the root.
func checkPattern(root, s string) error {
	if _, err := moqt.ParseGrantPattern(root, s); err != nil {
		return err
	}
	for seg := range strings.SplitSeq(s, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("segment %q is not allowed", seg)
		}
	}
	return nil
}
