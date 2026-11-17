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

// HLSDirectory is where a publisher opts a broadcast into the HLS egress:
// announcing /.hls/acme/cam asks it to serve /acme/cam.
const HLSDirectory = "/.hls/"

// ABRDirectory is where a publisher opts a broadcast into the ABR transcoder:
// announcing /.abr/acme/cam asks qumo abr to publish renditions of /acme/cam.
const ABRDirectory = "/.abr/"

// An OptIn names an egress a publisher opts a broadcast into by announcing it
// beneath the egress's directory. A key confined to a prefix signs for the
// prefix's mirror beneath the directory of each opt-in its key set entry
// holds (see [Key.OptIns]), so a control plane decides per key, per tenant,
// which egresses its publishers may use: an option it sells is held once
// paid for.
type OptIn string

// The opt-ins.
const (
	OptInHLS OptIn = "hls" // qumo hls, beneath HLSDirectory
	OptInABR OptIn = "abr" // qumo abr, beneath ABRDirectory
)

// Directory returns the directory o's opt-ins are announced beneath, or ""
// for an unknown opt-in.
func (o OptIn) Directory() string {
	switch o {
	case OptInHLS:
		return HLSDirectory
	case OptInABR:
		return ABRDirectory
	}
	return ""
}

// OptIns returns every opt-in.
func OptIns() []OptIn {
	return []OptIn{OptInHLS, OptInABR}
}

// ParseOptIns reads a comma-separated list of opt-ins, such as "hls,abr", in
// the canonical order OptIns returns them, each once. "" is none, nil. It
// refuses an unknown opt-in.
func ParseOptIns(s string) ([]OptIn, error) {
	var optIns []OptIn
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			optIns = append(optIns, OptIn(part))
		}
	}
	return normalizeOptIns(optIns)
}

// within reports whether root, a MoQ directory, lies at or beneath prefix,
// another one, or at or beneath prefix's mirror under the directory of one of
// optIns. Both are in Directory's form, so a plain prefix comparison holds at
// a "/" boundary; "/" contains every root.
func within(root, prefix string, optIns []OptIn) bool {
	if strings.HasPrefix(root, prefix) {
		return true
	}
	rel := strings.TrimPrefix(prefix, "/")
	for _, o := range optIns {
		if dir := o.Directory(); dir != "" && strings.HasPrefix(root, dir+rel) {
			return true
		}
	}
	return false
}
