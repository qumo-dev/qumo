package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
)

// Grant is what a session may do.
type Grant struct {
	// Publish is where the session may announce broadcasts.
	Publish Patterns
	// Subscribe is where the session may subscribe.
	Subscribe Patterns
	// expires is when the session must end; zero means never (public grant).
	expires time.Time
	// revalidate is how often to ask again; zero means never.
	revalidate time.Duration
}

// grantWire is the grant as the auth server sends it. root, mounts and peer
// are read only to refuse a grant that uses them.
type grantWire struct {
	Publish    []string        `json:"publish"`
	Subscribe  []string        `json:"subscribe"`
	Expires    *int64          `json:"expires"`
	Revalidate *int64          `json:"revalidate"`
	Root       string          `json:"root"`
	Mounts     json.RawMessage `json:"mounts"`
	Peer       bool            `json:"peer"`
}

func parseGrant(raw []byte, now time.Time) (*Grant, error) {
	var w grantWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidGrant, err)
	}
	if w.Root != "" || (len(w.Mounts) > 0 && string(w.Mounts) != "null" && string(w.Mounts) != "{}") || w.Peer {
		return nil, fmt.Errorf("%w: root, mounts and peer are not supported", ErrInvalidGrant)
	}
	publish, err := parsePatterns(w.Publish)
	if err != nil {
		return nil, fmt.Errorf("%w: publish: %w", ErrInvalidGrant, err)
	}
	subscribe, err := parsePatterns(w.Subscribe)
	if err != nil {
		return nil, fmt.Errorf("%w: subscribe: %w", ErrInvalidGrant, err)
	}
	if len(publish) == 0 && len(subscribe) == 0 {
		return nil, RefusedError{Status: http.StatusForbidden}
	}

	g := &Grant{Publish: publish, Subscribe: subscribe}
	if w.Expires != nil {
		g.expires = time.Unix(*w.Expires, 0)
		if !g.expires.After(now) {
			return nil, fmt.Errorf("%w: expires is not in the future", ErrInvalidGrant)
		}
	}
	if w.Revalidate != nil {
		if *w.Revalidate <= 0 || w.Expires == nil {
			return nil, fmt.Errorf("%w: revalidate needs a positive value and an expires", ErrInvalidGrant)
		}
		g.revalidate = time.Duration(*w.Revalidate) * time.Second
	}
	return g, nil
}

// Patterns is a set of subtree patterns.
type Patterns []pattern

// Contains reports whether any of the patterns contains path.
func (ps Patterns) Contains(path moqt.BroadcastPath) bool {
	for _, p := range ps {
		if p.contains(path) {
			return true
		}
	}
	return false
}

// pattern is a subtree pattern: "**" (everything) or "a/b/**" (a/b and
// everything beneath it, on "/" boundaries). base is "" for "**".
type pattern struct {
	base string
}

func parsePatterns(raw []string) (Patterns, error) {
	patterns := make(Patterns, 0, len(raw))
	for _, s := range raw {
		p, err := parsePattern(strings.TrimSpace(s))
		if err != nil {
			return nil, err
		}
		patterns = append(patterns, p)
	}
	return patterns, nil
}

func parsePattern(s string) (pattern, error) {
	if s == "**" {
		return pattern{}, nil
	}
	base, ok := strings.CutSuffix(s, "/**")
	if !ok {
		return pattern{}, fmt.Errorf("pattern %q: only subtree patterns (\"**\", \"a/b/**\") are supported", s)
	}
	for seg := range strings.SplitSeq(base, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.Contains(seg, "*") {
			return pattern{}, fmt.Errorf("pattern %q: bad segment %q", s, seg)
		}
	}
	return pattern{base: base}, nil
}

// contains reports whether path lies at or beneath the pattern's base. A
// broadcast path is rooted at "/"; patterns are relative to it.
func (p pattern) contains(path moqt.BroadcastPath) bool {
	if p.base == "" {
		return true
	}
	rel := strings.TrimPrefix(path.String(), "/")
	return rel == p.base || strings.HasPrefix(rel, p.base+"/")
}
