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

// Directory returns p as a MoQ directory, the form a grant's root and a key's
// prefix take: "acme/room", "/acme/room" and "/acme/room/" are all
// "/acme/room/", and "" is "/". It refuses a "." or ".." segment, and "*",
// "{" or "}" in a segment.
func Directory(p string) (string, error) {
	n, err := normalizePath(p)
	if err != nil {
		return "", err
	}
	if n == "" {
		return "/", nil
	}
	return "/" + n + "/", nil
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

// within reports whether root, a MoQ directory, lies at or beneath prefix,
// another one. Both are in Directory's form, so a plain prefix comparison
// holds at a "/" boundary; "/" contains every root.
func within(root, prefix string) bool {
	return strings.HasPrefix(root, prefix)
}
