package token

import (
	"fmt"
	"strings"
)

// normalizePath trims surrounding slashes and collapses repeated ones, as MoQ
// paths are compared. It refuses "." and ".." segments, which would let a
// path escape its prefix, and "*", which a relay reads as a pattern.
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

// within reports whether path lies at or beneath prefix at a "/" boundary; an
// empty prefix contains everything.
func within(path, prefix string) bool {
	return prefix == "" || path == prefix || strings.HasPrefix(path, prefix+"/")
}
