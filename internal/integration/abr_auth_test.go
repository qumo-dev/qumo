//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/gomoqt/msf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/abr"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/internal/optin"
	"github.com/qumo-dev/qumo/internal/relay"
	"github.com/qumo-dev/qumo/token"
)

// verifierFor returns a Verifier trusting the public halves of keys.
func verifierFor(t *testing.T, keys ...token.SigningKey) *auth.Verifier {
	t.Helper()
	public := make([]token.Key, len(keys))
	for i, k := range keys {
		public[i] = k.Public()
	}
	set, err := token.MarshalKeySet(public...)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "keys.json")
	require.NoError(t, os.WriteFile(path, set, 0o600))
	v, err := auth.NewVerifier(auth.VerifierConfig{Keys: path})
	require.NoError(t, err)
	return v
}

// TestABR_OptInNeedsItsOwnGrant runs the opt-in against a relay that
// verifies tokens: a tenant's key signs for its broadcasts but not for their
// opt-ins beneath /.abr/, which transcoding makes a paid option. A publisher
// whose credential reaches only its broadcast cannot announce the opt-in, so
// the transcoder, connected with its own credential (abr.RelayGrant), never
// transcodes it.
func TestABR_OptInNeedsItsOwnGrant(t *testing.T) {
	const (
		src = moqt.BroadcastPath("/live/paid")
		dst = moqt.BroadcastPath("/live/paid.abr")
	)
	optInPath := optin.Path(token.ABRDirectory, src)
	newKey := func(prefix string) token.SigningKey {
		k, err := token.GenerateKey(prefix)
		require.NoError(t, err)
		return k
	}
	tenant := newKey("/live/") // the tenant's own key
	operator := newKey("/")    // the transcoder's (RELAY_SIGNING_KEY)
	verifier := verifierFor(t, tenant, operator)
	addr, srv := startAuthRelay(t, verifier.Authorize, nil, func(s *relay.Server) { s.Tickets = verifier })
	sign := func(key token.SigningKey, g token.Grant) string {
		tok, err := token.Sign(key, g, time.Minute)
		require.NoError(t, err)
		return tok
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// The broadcast: a catalog listing one audio track.
	b, err := msf.NewBroadcast(msf.Catalog{Version: 1})
	require.NoError(t, err)
	require.NoError(t, b.RegisterTrack(msf.Track{
		Name: "audio", Packaging: msf.PackagingLOC, Role: msf.RoleAudio, IsLive: new(true),
		Codec: "opus", SampleRate: new(int64(48000)), ChannelConfig: "2",
	}, moqt.TrackHandlerFunc(func(tw *moqt.TrackWriter) { <-tw.Context().Done() })))
	pubMux := moqt.NewTrackMux(0)
	pubMux.Publish(ctx, src, b)
	pubMux.Publish(ctx, optInPath, optin.Handler(src))
	dialOver(t, "https://"+addr+"/?jwt="+sign(tenant, token.Grant{Root: "/live/", Publish: []string{"/paid"}}), nil, pubMux)
	require.Eventually(t, routed(srv, src), 5*time.Second, 25*time.Millisecond)

	// The transcoder, with the credential qumo abr signs for itself.
	abrMux := moqt.NewTrackMux(0)
	abrSess := dialOver(t, "https://"+addr+"/?jwt="+sign(operator, abr.RelayGrant()), nil, abrMux)
	tc := &abr.Transcoder{Suffix: ".abr", Config: abr.Config{
		Ladder:  []abr.Rung{{Height: 360, Bitrate: 800_000}},
		Encoder: abr.EncoderConfig{FFmpeg: "ffmpeg", Codec: "libx264"},
	}}
	go func() { _ = tc.Serve(ctx, abrSess, abrMux) }()

	// The tenant's key does not reach beneath /.abr/: it cannot even sign a
	// grant for the opt-in (and a relay refuses one forged without Sign, as
	// Verify checks the key's prefix too).
	_, err = token.Sign(tenant, token.Grant{Root: "/.abr/live/", Publish: []string{"/paid"}}, time.Minute)
	assert.ErrorIs(t, err, token.ErrForbidden)

	// So the opt-in, announced on a session the tenant's key admitted, is not
	// routed, and nothing is transcoded.
	assert.Never(t, func() bool { return routed(srv, optInPath)() || routed(srv, dst)() },
		2*time.Second, 25*time.Millisecond, "an opt-in was announced without a grant that reaches /.abr/")
}
