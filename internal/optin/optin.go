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

// Path returns the opt-in of broadcast beneath dir, an opt-in directory:
// Path("/.abr/", "/live/cam1") is "/.abr/live/cam1".
func Path(dir string, broadcast moqt.BroadcastPath) moqt.BroadcastPath {
	return moqt.BroadcastPath(strings.TrimSuffix(dir, "/") + "/" + strings.TrimPrefix(string(broadcast), "/"))
}

// Broadcast returns the broadcast an opt-in announced as optIn, beneath dir,
// asks an egress to serve: optIn with dir stripped.
//
//	Broadcast("/.hls/live/cam1", "/.hls/") is "/live/cam1"
//	Broadcast("/.hls/cam", "/.hls/")       is "/cam"
//	Broadcast("/.hls", "/.hls/")           is "/"
func Broadcast(optIn, dir string) string {
	p := strings.TrimPrefix(optIn, strings.TrimSuffix(dir, "/"))
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}
