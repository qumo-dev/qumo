package token

import (
	"fmt"
	"slices"
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

// HLSDirectory is where a publisher opts a broadcast into the HLS egress:
// announcing /.hls/acme/cam asks it to serve /acme/cam. A key confined to a
// prefix also signs for the prefix's mirror here (see [Key.Holds]), so the
// app that grants a broadcast grants its opt-in with the same key.
const HLSDirectory = "/.hls/"

// ABRDirectory is where a publisher opts a broadcast into the ABR transcoder:
// announcing /.abr/acme/cam asks qumo abr to publish renditions of /acme/cam.
// A key signs for the prefix's mirror here as it does under [HLSDirectory].
const ABRDirectory = "/.abr/"

// optInDirectories are the directories a publisher opts a broadcast into an
// egress beneath, each mirroring the whole namespace.
var optInDirectories = []string{HLSDirectory, ABRDirectory}

// OptInDirectories returns the directories a publisher opts a broadcast into
// an egress beneath: [HLSDirectory] and [ABRDirectory]. A key confined to a
// prefix also signs for the prefix's mirror under each.
func OptInDirectories() []string {
	return slices.Clone(optInDirectories)
}

// within reports whether root, a MoQ directory, lies at or beneath prefix,
// another one, or at or beneath prefix's mirror under an opt-in directory.
// Both are in Directory's form, so a plain prefix comparison holds at a "/"
// boundary; "/" contains every root.
func within(root, prefix string) bool {
	if strings.HasPrefix(root, prefix) {
		return true
	}
	rel := strings.TrimPrefix(prefix, "/")
	for _, dir := range optInDirectories {
		if strings.HasPrefix(root, dir+rel) {
			return true
		}
	}
	return false
}
