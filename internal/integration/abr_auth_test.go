//go:build integration

package integration

import (
	"context"
	"errors"
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

// verifierFor returns a Verifier trusting keys, as a key set lists them.
func verifierFor(t *testing.T, keys ...token.Key) *auth.Verifier {
	t.Helper()
	set, err := token.MarshalKeySet(keys...)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "keys.json")
	require.NoError(t, os.WriteFile(path, set, 0o600))
	v, err := auth.NewVerifier(auth.VerifierConfig{Keys: path})
	require.NoError(t, err)
	return v
}

// TestABR_PaidOptIn runs the opt-in against a relay that verifies tokens, as
// a control plane sells ABR: the tenant's key holds the ABR opt-in once the
// key set lists it (optIns), which the control plane sets when ABR is paid
// for. Unpaid, a ticket for the opt-in is refused, the opt-in is not routed
// and nothing is transcoded; paid, the same ticket is granted, the opt-in is
// routed and the transcoder, connected with its own credential
// (abr.RelayGrant), publishes the ABR broadcast.
func TestABR_PaidOptIn(t *testing.T) {
	tests := map[string]struct {
		optIns []token.OptIn // the key set's optIns for the tenant's key
		paid   bool
	}{
		"unpaid: the key set lists no ABR": {optIns: nil},
		"unpaid: an empty list":            {optIns: []token.OptIn{}},
		"paid: the key set lists ABR":      {optIns: []token.OptIn{token.OptInHLS, token.OptInABR}, paid: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			runPaidOptIn(t, tt.optIns, tt.paid)
		})
	}
}

func runPaidOptIn(t *testing.T, optIns []token.OptIn, paid bool) {
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
	tenant := newKey("/live/") // the tenant's key; its app signs every token below
	operator := newKey("/")    // the transcoder's (RELAY_SIGNING_KEY)
	tenantPub := tenant.Public()
	tenantPub.OptIns = optIns
	verifier := verifierFor(t, tenantPub, operator.Public())
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
	// Native QUIC, as TestRelay_Tickets uses: over WebTransport, a relay
	// closing a session that holds a granted ticket waits on its Auth
	// Stream's reader, which waits on the session to be gone.
	pub := dialOver(t, nativeURL(addr)+"/?jwt="+sign(tenant, token.Grant{Root: "/live/", Publish: []string{"/paid"}}), nil, pubMux)
	require.Eventually(t, routed(srv, src), 5*time.Second, 25*time.Millisecond)

	// The transcoder, with the credential qumo abr signs for itself.
	abrMux := moqt.NewTrackMux(0)
	abrSess := dialOver(t, "https://"+addr+"/?jwt="+sign(operator, abr.RelayGrant()), nil, abrMux)
	tc := &abr.Transcoder{Suffix: ".abr", Config: abr.Config{
		Ladder:  []abr.Rung{{Height: 360, Bitrate: 800_000}},
		Encoder: abr.EncoderConfig{FFmpeg: "ffmpeg", Codec: "libx264"},
	}}
	go func() { _ = tc.Serve(ctx, abrSess, abrMux) }()

	// The publisher's app signs a ticket for the opt-in with the tenant's
	// key, on the same session: whether the key holds it is the key set's.
	actx, acancel := context.WithTimeout(ctx, 3*time.Second)
	defer acancel()
	cred, err := pub.Auth(actx, []byte(sign(tenant, token.Grant{Root: "/.abr/live/", Publish: []string{"/paid"}})))
	if !paid {
		authErr, ok := errors.AsType[*moqt.AuthError](err)
		require.True(t, ok, "the ticket is refused with AUTH_ERROR, not %v", err)
		assert.Equal(t, moqt.UnauthorizedSessionErrorCode, authErr.Code)

		pubMux.Publish(ctx, optInPath, optin.Handler(src))
		assert.Never(t, func() bool { return routed(srv, optInPath)() || routed(srv, dst)() },
			2*time.Second, 25*time.Millisecond, "an opt-in was announced without ABR paid for")
		return
	}
	require.NoError(t, err, "the ticket is granted once ABR is paid for")
	t.Cleanup(func() { _ = cred.Close() })
	assert.Equal(t, "/.abr/live/", cred.Grant().Root)

	pubMux.Publish(ctx, optInPath, optin.Handler(src))
	require.Eventually(t, routed(srv, optInPath), 5*time.Second, 25*time.Millisecond)
	require.Eventually(t, routed(srv, dst), 10*time.Second, 50*time.Millisecond,
		"the transcoder, on its own credential, did not publish the ABR broadcast")
}
