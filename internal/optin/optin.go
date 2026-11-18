// Package optin serves and reads a publisher's opt-in to an egress: an
// announcement beneath an opt-in directory (token.HLSDirectory,
// token.ABRDirectory) that asks the egress to serve a broadcast. The opt-in
// for /live/cam1 is /.hls/live/cam1 or /.abr/live/cam1. It carries no media:
// it redirects every subscription to the broadcast, which the egress follows.
// It is kept apart from the egresses so a publisher need not import them.
package optin

import (
	"path"
	"strings"

	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/qumo-dev/qumo/token"
)

// Handler serves the opt-in of broadcast, announced as broadcast's path
// beneath an opt-in directory: it redirects every subscription to broadcast,
// keeping the track name, and refuses a subscriber that does not follow
// redirects with NOT_FOUND. An egress follows a redirect only to broadcast.
func Handler(broadcast moqt.BroadcastPath) moqt.TrackHandler {
	return moqt.TrackHandlerFunc(func(tw *moqt.TrackWriter) {
		if err := tw.Redirect(tw.Context(), moqt.Redirect{BroadcastPath: broadcast}); err != nil {
			tw.CloseWithError(moqt.SubscribeErrorCodeNotFound)
		}
	})
}

// Of returns the egress p opts a broadcast into, when p lies beneath an
// opt-in directory: token.OptInABR for /.abr/live/cam1. It reports false for
// a path beneath none.
func Of(p moqt.BroadcastPath) (token.OptIn, bool) {
	for _, o := range token.OptIns() {
		if strings.HasPrefix(string(p), o.Directory()) {
			return o, true
		}
	}
	return "", false
}

// Path returns the opt-in of broadcast beneath dir, an opt-in directory:
// Path("/.abr/", "/live/cam1") is "/.abr/live/cam1".
func Path(dir string, broadcast moqt.BroadcastPath) moqt.BroadcastPath {
	return moqt.BroadcastPath(strings.TrimSuffix(dir, "/") + "/" + strings.TrimPrefix(string(broadcast), "/"))
}

// Broadcast returns the broadcast an opt-in announced as optIn, beneath dir,
// asks an egress to serve: optIn with dir stripped, cleaned of doubled and
// trailing slashes.
//
//	Broadcast("/.hls/live/cam1", "/.hls/") is "/live/cam1"
//	Broadcast("/.hls/cam", "/.hls/")       is "/cam"
//	Broadcast("/.hls", "/.hls/")           is "/"
//
// It reports false for an opt-in with a "." or ".." segment. A relay grants
// and routes exact paths, so /.hls/acme/../victim/cam is announced under a
// grant for /.hls/acme/, yet cleans to /victim/cam, which an egress can read
// and would serve.
func Broadcast(optIn, dir string) (string, bool) {
	p := strings.TrimPrefix(optIn, strings.TrimSuffix(dir, "/"))
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if HasDotSegment(p) {
		return "", false
	}
	return path.Clean(p), true
}

// HasDotSegment reports whether p, a broadcast path, has a "." or ".."
// segment, which cleaning it would resolve into another path than the one a
// relay routes.
func HasDotSegment(p string) bool {
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}
