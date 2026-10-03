package authserver

import (
	"fmt"
	"strings"
	"time"
)

// grant is what a session may do, as the relay receives it: a subset of
// moq-auth's grant with subtree patterns only.
type grant struct {
	Publish   []string `json:"publish,omitempty"`
	Subscribe []string `json:"subscribe,omitempty"`
	// Expires is when the relay ends the session, in unix seconds: the
	// token's exp plus the leeway it was accepted within. Omitted for an
	// anonymous session, which has no token to expire.
	Expires int64 `json:"expires,omitzero"`
	// Revalidate is how often the relay asks again, in seconds; omitted when
	// nothing could change the answer (static keys).
	Revalidate int64 `json:"revalidate,omitzero"`
}

// grantFor turns verified claims into a grant. Every granted path must lie
// within the signing key's prefix at a "/" boundary: that is the rule that
// stops a registered key from signing for another tenant's paths.
func grantFor(c claims, key Key, revalidate time.Duration) (grant, error) {
	var g grant
	for _, role := range []struct {
		name   string
		suffix *string
		into   *[]string
	}{
		{"pub", c.PathAuth.Pub, &g.Publish},
		{"sub", c.PathAuth.Sub, &g.Subscribe},
	} {
		if role.suffix == nil {
			continue
		}
		scope, err := scopeOf(c.PathAuth.Root, *role.suffix)
		if err != nil {
			return grant{}, refuse("token: path_auth %s: %v", role.name, err)
		}
		if !within(scope, key.Prefix) {
			return grant{}, forbid("token: path_auth %s %q lies outside the signing key's prefix %q", role.name, scope, key.Prefix)
		}
		*role.into = []string{scope + "/**"}
	}
	if g.Publish == nil && g.Subscribe == nil {
		return grant{}, forbid("token: path_auth grants neither pub nor sub")
	}
	g.Expires = numericDate(*c.ExpiresAt).Add(leeway).Unix()
	g.Revalidate = int64(revalidate / time.Second)
	return g, nil
}

// scopeOf joins root and a role's suffix into the granted path. It must name
// a path: a grant of everything ("") is refused rather than read as "**".
func scopeOf(root, suffix string) (string, error) {
	r, err := normalizePath(root)
	if err != nil {
		return "", fmt.Errorf("root: %w", err)
	}
	s, err := normalizePath(suffix)
	if err != nil {
		return "", err
	}
	scope := strings.Trim(r+"/"+s, "/")
	if scope == "" {
		return "", fmt.Errorf("root and suffix name no path")
	}
	return scope, nil
}

// validPattern reports whether s is a subtree pattern the relay accepts: "**",
// or a path followed by "/**".
func validPattern(s string) bool {
	if s == "**" {
		return true
	}
	base, ok := strings.CutSuffix(s, "/**")
	if !ok {
		return false
	}
	norm, err := normalizePath(base)
	return err == nil && norm != "" && norm == base
}

// within reports whether path lies at or beneath prefix at a "/" boundary; an
// empty prefix contains everything.
func within(path, prefix string) bool {
	return prefix == "" || path == prefix || strings.HasPrefix(path, prefix+"/")
}

// normalizePath trims surrounding slashes and collapses repeated ones, as MoQ
// paths are compared. It refuses "." and ".." segments, which would let a path
// escape its prefix, and "*", which the relay reads as a pattern.
func normalizePath(p string) (string, error) {
	var kept []string
	for seg := range strings.SplitSeq(p, "/") {
		switch {
		case seg == "":
			continue
		case seg == "." || seg == "..":
			return "", fmt.Errorf("segment %q is not allowed", seg)
		case strings.Contains(seg, "*"):
			return "", fmt.Errorf("segment %q contains *", seg)
		}
		kept = append(kept, seg)
	}
	return strings.Join(kept, "/"), nil
}
