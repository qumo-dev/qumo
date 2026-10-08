package token

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Action is what a scope lets its bearer do.
type Action string

// The actions a scope may list. Any other is refused.
const (
	// ActionPublish is sending a track's groups: publishing it through a
	// relay, or recording into it at a funnel.
	ActionPublish Action = "publish"
	// ActionSubscribe is receiving a track live.
	ActionSubscribe Action = "subscribe"
	// ActionFetch is reading a track's history, such as a funnel's GET.
	ActionFetch Action = "fetch"
	// ActionAnnounce is announcing a broadcast.
	ActionAnnounce Action = "announce"
)

func (a Action) known() bool {
	switch a {
	case ActionPublish, ActionSubscribe, ActionFetch, ActionAnnounce:
		return true
	}
	return false
}

// Scope grants actions on the broadcasts and tracks it matches; everything a
// token's scopes don't grant is denied.
type Scope struct {
	// Actions are what the scope permits. At least one is required.
	Actions []Action
	// Broadcast is the broadcast path the scope reaches: that path alone,
	// or, with Prefix, it and every path beneath it on "/" boundaries. It
	// must lie within the signing key's prefix.
	Broadcast string
	Prefix    bool
	// Track is the one track name the scope reaches; empty reaches every
	// track. An action that names no track, such as announcing a broadcast,
	// is matched only by a scope with no Track.
	Track string
}

// Allows reports whether the scope permits action on the track named track
// of the broadcast at broadcast. An empty track is an action on the
// broadcast as a whole.
func (s Scope) Allows(action Action, broadcast, track string) bool {
	if s.Broadcast == "" || !slices.Contains(s.Actions, action) {
		return false
	}
	if s.Track != "" && s.Track != track {
		return false
	}
	// MoQ compares broadcast paths as written, so a path is matched only in
	// its one canonical spelling, relative to "/". Another spelling (a
	// doubled or trailing slash, a "." or ".." segment) names a different
	// broadcast and matches no scope.
	path, err := normalizePath(broadcast)
	if err != nil || path != strings.TrimPrefix(broadcast, "/") {
		return false
	}
	if s.Prefix {
		return within(path, s.Broadcast)
	}
	return path == s.Broadcast
}

// scopeJSON is a scope as encoded, the JSON counterpart of a CAT-4-MOQT
// scope: actions, a broadcast match (exact or prefix) and an optional track
// match (exact).
type scopeJSON struct {
	Actions   []Action        `json:"actions"`
	Broadcast *broadcastMatch `json:"broadcast"`
	Track     *trackMatch     `json:"track,omitzero"`
}

type broadcastMatch struct {
	Exact  *string `json:"exact,omitzero"`
	Prefix *string `json:"prefix,omitzero"`
}

type trackMatch struct {
	Exact *string `json:"exact"`
}

// encodeScopes encodes scopes for the scopes claim, checking each as Verify
// would.
func encodeScopes(scopes []Scope, keyPrefix string) (jsontext.Value, error) {
	out := make([]scopeJSON, len(scopes))
	for i, s := range scopes {
		path, err := normalizePath(s.Broadcast)
		if err != nil || path == "" {
			return nil, fmt.Errorf("token: scope %d: broadcast %q: want a path with no \".\", \"..\" or \"*\" segments", i, s.Broadcast)
		}
		if !within(path, keyPrefix) {
			return nil, fmt.Errorf("token: scope %d: broadcast %q lies outside the signing key's prefix %q", i, path, keyPrefix)
		}
		if len(s.Actions) == 0 {
			return nil, fmt.Errorf("token: scope %d lists no actions", i)
		}
		for _, a := range s.Actions {
			if !a.known() {
				return nil, fmt.Errorf("token: scope %d: unknown action %q", i, a)
			}
		}
		sj := scopeJSON{Actions: s.Actions, Broadcast: &broadcastMatch{Exact: &path}}
		if s.Prefix {
			sj.Broadcast = &broadcastMatch{Prefix: &path}
		}
		if s.Track != "" {
			sj.Track = &trackMatch{Exact: &s.Track}
		}
		out[i] = sj
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("token: encode scopes: %w", err)
	}
	return raw, nil
}

// scopesOf decodes the scopes claim into the scopes it grants, each confined
// to prefix at a "/" boundary. A member it doesn't know is refused rather
// than ignored, since ignoring a misspelled track match would widen a scope
// to every track. No scopes grant nothing, which is forbidden.
func scopesOf(raw jsontext.Value, prefix string) ([]Scope, error) {
	var encoded []scopeJSON
	if err := json.Unmarshal(raw, &encoded, json.RejectUnknownMembers(true)); err != nil {
		return nil, invalid("scopes: %v", err)
	}
	if len(encoded) == 0 {
		return nil, forbidden("scopes grant nothing")
	}
	scopes := make([]Scope, len(encoded))
	for i, sj := range encoded {
		s, err := sj.scope()
		if err != nil {
			return nil, invalid("scope %d: %v", i, err)
		}
		if !within(s.Broadcast, prefix) {
			return nil, forbidden("scope %d: broadcast %q lies outside the signing key's prefix %q", i, s.Broadcast, prefix)
		}
		scopes[i] = s
	}
	return scopes, nil
}

func (sj scopeJSON) scope() (Scope, error) {
	if len(sj.Actions) == 0 {
		return Scope{}, errors.New("no actions")
	}
	for _, a := range sj.Actions {
		if !a.known() {
			return Scope{}, fmt.Errorf("unknown action %q", a)
		}
	}
	var s Scope
	switch b := sj.Broadcast; {
	case b == nil:
		return Scope{}, errors.New("no broadcast match")
	case (b.Exact == nil) == (b.Prefix == nil):
		return Scope{}, errors.New("the broadcast match wants exactly one of exact and prefix")
	case b.Exact != nil:
		s.Broadcast = *b.Exact
	default:
		s.Broadcast, s.Prefix = *b.Prefix, true
	}
	path, err := normalizePath(s.Broadcast)
	if err != nil {
		return Scope{}, fmt.Errorf("broadcast: %w", err)
	}
	if path == "" {
		return Scope{}, errors.New("broadcast names no path")
	}
	s.Broadcast = path
	if sj.Track != nil {
		if sj.Track.Exact == nil || *sj.Track.Exact == "" {
			return Scope{}, errors.New("the track match wants a non-empty exact")
		}
		s.Track = *sj.Track.Exact
	}
	s.Actions = sj.Actions
	return s, nil
}
