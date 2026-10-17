package ingest

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
)

// PublishAuthorizer decides whether a pushed stream may publish the broadcast
// at path with credential, the ?jwt= the publisher presented ("" for none),
// and returns when that permission lapses: the stream is cut then. The zero
// time never lapses. An error refuses the stream.
type PublishAuthorizer func(credential string, path moqt.BroadcastPath) (lapses time.Time, err error)

// splitCredential splits a name such as "cam?jwt=TOKEN" into the name and the
// credential its query carries as jwt, "" for none.
func splitCredential(s string) (name, credential string) {
	name, rawQuery, _ := strings.Cut(s, "?")
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		// A query that doesn't parse carries no credential: the publish is
		// then checked as one without.
		return name, ""
	}
	return name, query.Get("jwt")
}

// rtmpTarget returns the broadcast an RTMP publish names, /app/streamKey, and
// the credential it carries: ?jwt= on the stream key (OBS's "Stream Key"
// field), or else on the app (rtmp://host/live?jwt=… puts it there). Neither
// query is part of the path.
func rtmpTarget(app, streamKey string) (moqt.BroadcastPath, string) {
	appName, appCredential := splitCredential(app)
	key, credential := splitCredential(streamKey)
	if credential == "" {
		credential = appCredential
	}
	return moqt.BroadcastPath("/" + appName + "/" + key), credential
}

// untilLapse returns ctx ended at lapses as well, or ctx alone for the zero
// time.
func untilLapse(ctx context.Context, lapses time.Time) (context.Context, context.CancelFunc) {
	if lapses.IsZero() {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, lapses)
}
