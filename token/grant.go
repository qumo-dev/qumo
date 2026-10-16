package token

import (
	"fmt"
	"slices"
	"strings"

	"github.com/qumo-dev/gomoqt/moqt"
)

// Action is something a grant may let its bearer do on a broadcast path.
type Action string

// The actions a grant names patterns for.
const (
	// ActionPublish is sending a broadcast's tracks through a relay.
	ActionPublish Action = "publish"
	// ActionSubscribe is receiving a track live.
	ActionSubscribe Action = "subscribe"
	// ActionFetch is reading a track's history, such as a funnel's GET.
	ActionFetch Action = "fetch"
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

// Grant is what a token allows its bearer, in the shape of a MoQ grant
// (moqt.Grant): a root, the directory of broadcast paths it applies to, and
// for each action the path patterns beneath it the bearer may act on. A
// pattern is read as moqt.Grant reads it, relative to the root: with Root
// "acme/room/42", "/7" is the broadcast "/acme/room/42/7" alone, "/7/" every
// broadcast beneath it, "/" every broadcast beneath the root, and a segment
// may be a wildcard ("/{user}/audio"). Whatever no pattern matches is denied.
//
// In a token, Root lies at or beneath the signing key's prefix, so every
// pattern does too. A grant with no patterns grants nothing: it names its
// key's tenant alone, as a relay session's credential does when its grants
// come later.
type Grant struct {
	// Root is a broadcast path without a leading or trailing "/", such as
	// "acme/room/42"; empty is the root of every path.
	Root string
	// Publish matches the broadcasts the bearer may publish through a relay.
	Publish []string
	// Subscribe matches the broadcasts the bearer may receive live.
	Subscribe []string
	// Fetch matches the broadcasts whose history the bearer may read.
	Fetch []string
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
	case ActionFetch:
		return g.Fetch
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
	return []Action{ActionPublish, ActionSubscribe, ActionFetch, ActionCreate, ActionPost, ActionRedact}
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
	root := moqRoot(g.Root)
	for _, s := range g.Patterns(action) {
		pattern, err := moqt.ParseGrantPattern(root, s)
		if err == nil && pattern.Match(bp) {
			return true
		}
	}
	return false
}

// MoQ returns what the grant lets a relay session do, as a MoQ grant: its
// root, and its publish and subscribe patterns unchanged. Every other action
// is not the relay's, and a grant with no publish or subscribe pattern
// grants a relay session nothing (moqt.Grant{}).
func (g Grant) MoQ() moqt.Grant {
	if len(g.Publish) == 0 && len(g.Subscribe) == 0 {
		return moqt.Grant{}
	}
	return moqt.Grant{
		Root:      moqRoot(g.Root),
		Publish:   slices.Clone(g.Publish),
		Subscribe: slices.Clone(g.Subscribe),
	}
}

// moqRoot is root as a MoQ grant's root: "/acme/room/42/", or "/" for the
// empty root.
func moqRoot(root string) string {
	if root == "" {
		return "/"
	}
	return "/" + root + "/"
}

// checked returns g with its root normalized, refusing a root that isn't a
// path (ErrInvalid) or lies outside prefix (ErrForbidden), and a pattern
// moqt.ParseGrantPattern refuses or holding a "." or ".." segment
// (ErrInvalid).
func (g Grant) checked(prefix string) (Grant, error) {
	root, err := normalizePath(g.Root)
	if err != nil {
		return Grant{}, invalid("root %q: %v", g.Root, err)
	}
	g.Root = root
	patterns := false
	for _, a := range Actions() {
		for _, s := range g.Patterns(a) {
			if err := checkPattern(root, s); err != nil {
				return Grant{}, invalid("%s pattern %q: %v", a, s, err)
			}
			patterns = true
		}
	}
	// A grant of no patterns may leave its root out: it names its key's
	// tenant alone.
	if (root != "" || patterns) && !within(root, prefix) {
		return Grant{}, forbidden("root %q lies outside the signing key's prefix %q", root, prefix)
	}
	return g, nil
}

// checkPattern checks s as a pattern relative to root. A "." or ".."
// segment is refused: MoQ would read it literally, while a path elsewhere,
// such as a URL, might resolve it out of the root.
func checkPattern(root, s string) error {
	if _, err := moqt.ParseGrantPattern(moqRoot(root), s); err != nil {
		return err
	}
	for seg := range strings.SplitSeq(s, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("segment %q is not allowed", seg)
		}
	}
	return nil
}
