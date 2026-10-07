package auth

import (
	"fmt"
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
	// revalidate is how often to check again; zero means never.
	revalidate time.Duration
}

// NewGrant returns a grant for the subtree patterns publish and subscribe
// ("**" for everything, "a/b/**" for a/b and everything beneath it). The
// session ends at expires (zero: never) and is checked again every revalidate
// (zero: never). It is for an Authorize other than the Verifier's.
func NewGrant(publish, subscribe []string, expires time.Time, revalidate time.Duration) (*Grant, error) {
	pub, err := parsePatterns(publish)
	if err != nil {
		return nil, fmt.Errorf("publish: %w", err)
	}
	sub, err := parsePatterns(subscribe)
	if err != nil {
		return nil, fmt.Errorf("subscribe: %w", err)
	}
	return &Grant{Publish: pub, Subscribe: sub, expires: expires, revalidate: revalidate}, nil
}

// Expires returns when the session must end: the credential's expiry. The
// zero Time means the grant does not expire.
func (g *Grant) Expires() time.Time {
	return g.expires
}

// Revalidate returns how often the relay checks a live session again. Zero
// means never.
func (g *Grant) Revalidate() time.Duration {
	return g.revalidate
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

// Bases returns the path each pattern covers, relative to "/": "a/b" for
// "a/b/**", and "" for "**".
func (ps Patterns) Bases() []string {
	bases := make([]string, len(ps))
	for i, p := range ps {
		bases[i] = p.base
	}
	return bases
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
