// Package hlsoptin serves a publisher's opt-in to the HLS egress: the
// announcement beneath token.HLSDirectory that asks qumo hls to serve a
// broadcast. It is kept apart from the egress so a publisher need not import
// it.
package hlsoptin

import "github.com/qumo-dev/gomoqt/moqt"

// Handler serves the opt-in of broadcast, announced as broadcast's path
// beneath token.HLSDirectory: it redirects every subscription to broadcast,
// keeping the track name, and refuses a subscriber that does not follow
// redirects with NOT_FOUND. The egress follows a redirect only to broadcast.
func Handler(broadcast moqt.BroadcastPath) moqt.TrackHandler {
	return moqt.TrackHandlerFunc(func(tw *moqt.TrackWriter) {
		if err := tw.Redirect(tw.Context(), moqt.Redirect{BroadcastPath: broadcast}); err != nil {
			tw.CloseWithError(moqt.SubscribeErrorCodeNotFound)
		}
	})
}
