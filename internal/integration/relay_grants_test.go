//go:build integration

// Black-box tests of the relay enforcing a credential's grant, verified by a
// real auth.Verifier: a publish pattern matching a broadcast lets a session
// announce it, and subscribe patterns gate the broadcasts a viewer receives.
package integration

import (
	"context"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/token"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// subscribeTrack subscribes to track at path over a new session to url, and
// returns nil once a group arrives, or the error that refused it.
func subscribeTrack(t *testing.T, url string, path moqt.BroadcastPath, track moqt.TrackName) error {
	t.Helper()
	sess := dialOver(t, url, nil, moqt.NewTrackMux(0))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, err := sess.Subscribe(ctx, path, track, nil)
	if err != nil {
		return err
	}
	defer tr.Close()
	_, err = tr.AcceptGroup(ctx)
	return err
}

func TestRelay_PatternCredentials(t *testing.T) {
	key, _, keysFile := writeKeys(t)
	verifier, err := auth.NewVerifier(auth.VerifierConfig{Keys: keysFile})
	require.NoError(t, err)
	addr, srv := startAuthRelay(t, verifier.Authorize, nil)
	url := func(g token.Grant) string {
		c, err := token.Sign(key, g, time.Minute)
		require.NoError(t, err)
		return "https://" + addr + "/?jwt=" + c
	}
	publishOver(t, srv, url(exactly("acme/live", token.ActionPublish)), "/acme/live")
	viewer := url(grantOn("acme", []string{"/"}, token.ActionSubscribe))

	t.Run("a viewer of the broadcast", func(t *testing.T) {
		assert.NoError(t, subscribeTrack(t, viewer, "/acme/live", "video"))
	})
	t.Run("a viewer granted a wildcard", func(t *testing.T) {
		err := subscribeTrack(t, url(grantOn("acme", []string{"/{name}"}, token.ActionSubscribe)), "/acme/live", "video")

		assert.NoError(t, err)
	})
	t.Run("a viewer granted the root alone", func(t *testing.T) {
		err := subscribeTrack(t, url(grantOn("acme/live", []string{"/"}, token.ActionSubscribe)), "/acme/live", "video")

		assert.Error(t, err, "/ matches every path beneath the root, not the root itself")
	})
	t.Run("a viewer granted another broadcast", func(t *testing.T) {
		err := subscribeTrack(t, url(exactly("acme/other", token.ActionSubscribe)), "/acme/live", "video")

		assert.Error(t, err)
	})
	t.Run("a viewer granted the exact broadcast", func(t *testing.T) {
		err := subscribeTrack(t, url(exactly("acme/live", token.ActionSubscribe)), "/acme/live", "video")

		assert.NoError(t, err)
	})
	t.Run("no publish pattern for the broadcast announces nothing", func(t *testing.T) {
		tests := map[string]struct {
			path  moqt.BroadcastPath
			grant token.Grant
		}{
			"subscribe only": {
				path:  "/acme/quiet",
				grant: exactly("acme/quiet", token.ActionSubscribe),
			},
			"publish on another broadcast": {
				path:  "/acme/elsewhere",
				grant: grantOn("acme/other", []string{"/"}, token.ActionPublish),
			},
			// Post writes into a broadcast through a funnel; the broadcast is
			// someone else's to announce.
			"post only": {
				path:  "/acme/comments",
				grant: exactly("acme/comments", token.ActionPost),
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				mux := moqt.NewTrackMux(0)
				mux.PublishFunc(ctx, tt.path, func(tw *moqt.TrackWriter) { <-tw.Context().Done() })

				dialOver(t, url(tt.grant), nil, mux)

				assert.Never(t, routed(srv, tt.path), time.Second, 50*time.Millisecond)
			})
		}
	})
}
