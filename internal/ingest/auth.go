package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/qumo-dev/qumo/internal/optin"
	"github.com/qumo-dev/qumo/token"
)

// PublishAuthorizer decides whether a pushed stream may publish the broadcast
// at path with credential, a ?jwt= the publisher presented ("" for none),
// and returns the permission it grants. An error refuses the stream.
//
// A stream's opt-ins (see [optIn]) are asked for as well, with path the
// opt-in's, such as /.abr/live/cam for /live/cam.
type PublishAuthorizer func(credential string, path moqt.BroadcastPath) (Permission, error)

// A Permission is what a PublishAuthorizer grants a push.
type Permission struct {
	// Lapses is when the permission lapses: the stream is cut then. The zero
	// time never lapses.
	Lapses time.Time
	// KeyID is the kid of the key that signed the credential granting it,
	// whose usage the push is (see [RTMPConfig.Meter]), or "" when none was
	// checked.
	KeyID string
}

// splitCredential splits a name such as "cam?jwt=A&jwt=B" into the name and
// the credentials its query carries as jwt, in order, none for none.
func splitCredential(s string) (name string, credentials []string) {
	name, rawQuery, _ := strings.Cut(s, "?")
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		// A query that doesn't parse carries no credential: the publish is
		// then checked as one without.
		return name, nil
	}
	return name, query["jwt"]
}

// rtmpTarget returns the broadcast an RTMP publish names, /app/streamKey, and
// the credentials it carries: each ?jwt= on the stream key (OBS's "Stream
// Key" field), then each on the app (rtmp://host/live?jwt=… puts it there).
// Neither query is part of the path.
func rtmpTarget(app, streamKey string) (moqt.BroadcastPath, []string) {
	appName, appCredentials := splitCredential(app)
	key, credentials := splitCredential(streamKey)
	return moqt.BroadcastPath("/" + appName + "/" + key), append(credentials, appCredentials...)
}

// errOptInPath refuses a push to a path beneath an opt-in directory, such as
// /.abr/live/cam: the server announces opt-ins there on publishers' behalf,
// and media there would answer an egress that expects a redirect.
var errOptInPath = errors.New("the path lies beneath an opt-in directory, where the server opts streams in: push to the broadcast's own path")

// checkPushPath refuses a push to path when the server keeps it for opt-ins
// (errOptInPath).
func checkPushPath(path moqt.BroadcastPath) error {
	if _, ok := optin.Of(path); ok {
		return fmt.Errorf("%s: %w", path, errOptInPath)
	}
	return nil
}

// authorizeAny asks authorize whether path may be published with one of
// credentials, in order, and returns the first permission granted. Without
// credentials it asks for a publish that presented none. When none is
// granted it returns the first refusal.
func authorizeAny(authorize PublishAuthorizer, credentials []string, path moqt.BroadcastPath) (Permission, error) {
	if len(credentials) == 0 {
		return authorize("", path)
	}
	var first error
	for _, credential := range credentials {
		p, err := authorize(credential, path)
		if err == nil {
			return p, nil
		}
		if first == nil {
			first = err
		}
	}
	return Permission{}, first
}

// optIn opts sess's broadcast into each of optIns, the egresses the server
// offers, that a publisher with credentials may publish the opt-in of:
// authorize is asked for the opt-in's path, beneath the egress's directory,
// and a nil authorize accepts every one. Each opt-in is withdrawn when its
// permission lapses or sess closes; a refused one leaves the broadcast as it
// is.
func optIn(sess *Session, authorize PublishAuthorizer, credentials []string, optIns []token.OptIn) {
	for _, o := range optIns {
		path := optin.Path(o.Directory(), sess.path)
		var granted Permission
		if authorize != nil {
			var err error
			if granted, err = authorizeAny(authorize, credentials, path); err != nil {
				slog.Info("ingest opt-in refused", "broadcast_path", sess.path, "opt_in", o, "error", err)
				continue
			}
		}
		sess.OptIn(o, granted.Lapses)
	}
}

// untilLapse returns ctx ended at lapses as well, or ctx alone for the zero
// time.
func untilLapse(ctx context.Context, lapses time.Time) (context.Context, context.CancelFunc) {
	if lapses.IsZero() {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, lapses)
}
