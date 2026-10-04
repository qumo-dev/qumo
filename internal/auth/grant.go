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
	// expires is when the session must end; zero means never.
	expires time.Time
	// revalidate is how often to ask again; zero means never.
	revalidate time.Duration
}

// UnmarshalJSON decodes a grant as the auth server sends it. root, mounts and
// peer are read only to refuse a grant that uses them.
func (g *Grant) UnmarshalJSON(b []byte) error {
	var w struct {
		Publish    []string        `json:"publish"`
		Subscribe  []string        `json:"subscribe"`
		Expires    *int64          `json:"expires"`
		Revalidate *int64          `json:"revalidate"`
		Root       string          `json:"root"`
		Mounts     json.RawMessage `json:"mounts"`
		Peer       bool            `json:"peer"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	if w.Root != "" || (len(w.Mounts) > 0 && string(w.Mounts) != "null" && string(w.Mounts) != "{}") || w.Peer {
		return fmt.Errorf("root, mounts and peer are not supported")
	}
	publish, err := parsePatterns(w.Publish)
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	subscribe, err := parsePatterns(w.Subscribe)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	*g = Grant{Publish: publish, Subscribe: subscribe}
	if w.Expires != nil {
		g.expires = time.Unix(*w.Expires, 0)
	}
	if w.Revalidate != nil {
		if *w.Revalidate <= 0 || w.Expires == nil {
			return fmt.Errorf("revalidate needs a positive value and an expires")
		}
		g.revalidate = time.Duration(*w.Revalidate) * time.Second
	}
	return nil
}

// Expires returns when the session must end, which the auth server sets to the
// credential's expiry. The zero Time means the grant does not expire.
func (g *Grant) Expires() time.Time {
	return g.expires
}

// Revalidate returns how often the relay asks the auth server again about a
// live session. Zero means never.
func (g *Grant) Revalidate() time.Duration {
	return g.revalidate
}

// parseGrant decodes an auth server's grant and checks it can be enforced at
// now. A grant that names nothing is a refusal.
func parseGrant(raw []byte, now time.Time) (*Grant, error) {
	var g Grant
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidGrant, err)
	}
	if len(g.Publish) == 0 && len(g.Subscribe) == 0 {
		return nil, RefusedError{Status: http.StatusForbidden}
	}
	if !g.expires.IsZero() && !g.expires.After(now) {
		return nil, fmt.Errorf("%w: expires is not in the future", ErrInvalidGrant)
	}
	return &g, nil
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
