//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/token"
)

// The relay grants and routes a broadcast by its exact path: a grant for
// /.hls/acme/ announces /.hls/acme/../victim/cam, which cleans to an opt-in
// for /victim/cam. So the HLS egress, which may read every broadcast, must
// refuse an opt-in path with a "." or ".." segment rather than clean it
// (optin.Broadcast).
func TestRelay_RoutesOptInDotSegmentsAsGranted(t *testing.T) {
	key, _, keysFile := writeKeys(t)
	verifier, err := auth.NewVerifier(auth.VerifierConfig{Keys: keysFile})
	require.NoError(t, err)
	addr, srv := startAuthRelay(t, verifier.Authorize, nil)
	credential, err := token.Sign(key, grantOn(".hls/acme", []string{"/"}, token.ActionPublish), time.Minute)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	const optIn = moqt.BroadcastPath("/.hls/acme/../victim/cam")
	mux := moqt.NewTrackMux(0)
	mux.PublishFunc(ctx, optIn, func(tw *moqt.TrackWriter) { <-tw.Context().Done() })
	dialOver(t, "https://"+addr+"/?jwt="+credential, nil, mux)

	assert.Eventually(t, routed(srv, optIn), 3*time.Second, 50*time.Millisecond)
}
