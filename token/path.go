package token

import (
	"fmt"
	"strings"
)

// normalizePath trims surrounding slashes and collapses repeated ones, as MoQ
// paths are compared. It refuses "." and ".." segments, which would let a
// path escape its prefix, and "*", "{" and "}", which a relay reads as a
// pattern.
func normalizePath(p string) (string, error) {
	var kept []string
	for seg := range strings.SplitSeq(p, "/") {
		switch {
		case seg == "":
			continue
		case seg == "." || seg == "..":
			return "", fmt.Errorf("segment %q is not allowed", seg)
		case strings.ContainsAny(seg, "*{}"):
			return "", fmt.Errorf("segment %q contains *, { or }", seg)
		}
		kept = append(kept, seg)
	}
	return strings.Join(kept, "/"), nil
}

// atAndBeneath returns the patterns, relative to a root, matching the path
// suffix and every path beneath it, or everything beneath the root ("/") for
// an empty suffix.
func atAndBeneath(suffix string) ([]string, error) {
	s, err := normalizePath(suffix)
	if err != nil {
		return nil, err
	}
	if s == "" {
		return []string{"/"}, nil
	}
	return []string{"/" + s, "/" + s + "/"}, nil
}

// within reports whether path lies at or beneath prefix at a "/" boundary; an
// empty prefix contains everything. A key's prefix confines a grant's root
// this way.
func within(path, prefix string) bool {
	return prefix == "" || path == prefix || strings.HasPrefix(path, prefix+"/")
}
