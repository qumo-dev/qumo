//go:build integration

// Black-box tests of the relay enforcing a scoped credential, verified by a
// real auth.Verifier: a publish scope on every track of a broadcast lets a
// session announce it, and a subscribe scope gates the tracks a viewer
// receives.
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

func TestRelay_ScopedCredentials(t *testing.T) {
	key, _, keysFile := writeKeys(t)
	verifier, err := auth.NewVerifier(auth.VerifierConfig{Keys: keysFile})
	require.NoError(t, err)
	addr, srv := startAuthRelay(t, verifier.Authorize, nil)
	url := func(scopes ...token.Scope) string {
		c, err := token.Sign(key, token.Grant{Scopes: scopes}, time.Minute)
		require.NoError(t, err)
		return "https://" + addr + "/?jwt=" + c
	}
	scope := func(broadcast string, prefix bool, track string, actions ...token.Action) token.Scope {
		return token.Scope{Actions: actions, Broadcast: broadcast, Prefix: prefix, Track: track}
	}
	publishOver(t, srv, url(scope("acme/live", false, "", token.ActionPublish)), "/acme/live")
	viewer := url(scope("acme", true, "", token.ActionSubscribe))

	t.Run("a viewer of the broadcast", func(t *testing.T) {
		assert.NoError(t, subscribeTrack(t, viewer, "/acme/live", "video"))
	})
	t.Run("a viewer scoped to another track", func(t *testing.T) {
		err := subscribeTrack(t, url(scope("acme/live", false, "audio", token.ActionSubscribe)), "/acme/live", "video")

		assert.Error(t, err)
	})
	t.Run("a viewer scoped to the exact track", func(t *testing.T) {
		err := subscribeTrack(t, url(scope("acme/live", false, "video", token.ActionSubscribe)), "/acme/live", "video")

		assert.NoError(t, err)
	})
	t.Run("fetch does not grant subscribing", func(t *testing.T) {
		err := subscribeTrack(t, url(scope("acme", true, "", token.ActionFetch)), "/acme/live", "video")

		assert.Error(t, err)
	})
	t.Run("no publish scope on every track of the broadcast announces nothing", func(t *testing.T) {
		tests := map[string]struct {
			path   moqt.BroadcastPath
			scopes []token.Scope
		}{
			"subscribe and fetch only": {
				path:   "/acme/quiet",
				scopes: []token.Scope{scope("acme/quiet", false, "", token.ActionSubscribe, token.ActionFetch)},
			},
			"publish on another broadcast": {
				path:   "/acme/elsewhere",
				scopes: []token.Scope{scope("acme/other", true, "", token.ActionPublish)},
			},
			// A scope naming one track writes into a broadcast through a
			// funnel; the broadcast is someone else's to announce.
			"publish on one track only": {
				path:   "/acme/comments",
				scopes: []token.Scope{scope("acme/comments", false, "chat", token.ActionPublish)},
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				mux := moqt.NewTrackMux(0)
				mux.PublishFunc(ctx, tt.path, func(tw *moqt.TrackWriter) { <-tw.Context().Done() })

				dialOver(t, url(tt.scopes...), nil, mux)

				assert.Never(t, routed(srv, tt.path), time.Second, 50*time.Millisecond)
			})
		}
	})
}
