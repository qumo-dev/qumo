//go:build integration

// Black-box tests of tickets on a real QUIC/MOQT relay that verifies
// credentials: a session connected with a credential that grants nothing
// presents tickets on Auth Streams, and may do what they grant while they
// are in force.
package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/internal/relay"
	"github.com/qumo-dev/qumo/token"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// trustedKeys returns n keys of the tenant prefix and a Verifier trusting
// every one of them.
func trustedKeys(t *testing.T, prefix string, n int) ([]token.SigningKey, *auth.Verifier) {
	t.Helper()
	keys := make([]token.SigningKey, n)
	public := make([]token.Key, n)
	for i := range keys {
		k, err := token.GenerateKey(prefix)
		require.NoError(t, err)
		keys[i], public[i] = k, k.Public()
	}
	set, err := token.MarshalKeySet(public...)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "keys.json")
	require.NoError(t, os.WriteFile(path, set, 0o600))
	v, err := auth.NewVerifier(auth.VerifierConfig{Keys: path})
	require.NoError(t, err)
	return keys, v
}

// subscribeOn subscribes to testTrack at path over sess, and returns nil once
// a group arrives, or the error that refused it.
func subscribeOn(t *testing.T, sess *moqt.Session, path moqt.BroadcastPath) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tr, err := sess.Subscribe(ctx, path, testTrack, nil)
	if err != nil {
		return err
	}
	defer tr.Close()
	_, err = tr.AcceptGroup(ctx)
	return err
}

// announceOn announces path over mux, an empty broadcast, until the returned
// func ends it.
func announceOn(mux *moqt.TrackMux, path moqt.BroadcastPath) func() {
	ann, end := moqt.NewAnnouncement(context.Background(), path)
	mux.Announce(ann, moqt.NewBroadcast())
	return end
}

func TestRelay_Tickets(t *testing.T) {
	keys, verifier := trustedKeys(t, "acme", 2)
	session, other := keys[0], keys[1]
	addr, srv := startAuthRelay(t, verifier.Authorize, nil, func(s *relay.Server) { s.Tickets = verifier })
	sign := func(key token.SigningKey, g token.Grant) string {
		tok, err := token.Sign(key, g, time.Minute)
		require.NoError(t, err)
		return tok
	}
	publishOver(t, srv, "https://"+addr+"/?jwt="+sign(session, atAndBeneath("acme/room/42/host", token.ActionPublish)),
		"/acme/room/42/host")
	// A room: listen to everything in it, and speak as one member.
	ticket := sign(session, token.Grant{
		Root:      "acme/room/42",
		Publish:   []string{"/7"},
		Subscribe: []string{"/"},
		Fetch:     []string{"/"},
	})
	mux := moqt.NewTrackMux(0)
	sess := dialOver(t, nativeURL(addr)+"/?jwt="+sign(session, token.Grant{}), nil, mux)
	present := func(tok string) (*moqt.Credential, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return sess.Auth(ctx, []byte(tok))
	}

	// The credential the session connected with grants nothing.
	assert.True(t, isSubscribeRefused(subscribeOn(t, sess, "/acme/room/42/host"), moqt.SubscribeErrorCodeUnauthorized),
		"no subscription before a ticket")
	endEarly := announceOn(mux, "/acme/room/42/7")
	assert.Never(t, routed(srv, "/acme/room/42/7"), 500*time.Millisecond, 25*time.Millisecond, "no announcement before a ticket")
	endEarly()

	_, err := present(sign(other, token.Grant{Root: "acme/room/42", Subscribe: []string{"/"}}))
	authErr, ok := errors.AsType[*moqt.AuthError](err)
	require.True(t, ok, "a ticket of another trusted key is refused with AUTH_ERROR, not %v", err)
	assert.Equal(t, moqt.UnauthorizedSessionErrorCode, authErr.Code)

	cred, err := present(ticket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cred.Close() })
	assert.Equal(t, "/acme/room/42/", cred.Grant().Root)

	t.Run("subscribes within the ticket", func(t *testing.T) {
		assert.NoError(t, subscribeOn(t, sess, "/acme/room/42/host"))
	})
	t.Run("announces the broadcast it may publish", func(t *testing.T) {
		// Announced anew: one announced while refused stays dropped.
		end := announceOn(mux, "/acme/room/42/7")
		t.Cleanup(end)
		endOther := announceOn(mux, "/acme/room/42/8")
		t.Cleanup(endOther)

		require.Eventually(t, routed(srv, "/acme/room/42/7"), 3*time.Second, 25*time.Millisecond)
		assert.Never(t, routed(srv, "/acme/room/42/8"), 500*time.Millisecond, 25*time.Millisecond,
			"a broadcast in the room it may not publish")
	})
	t.Run("lists announcements within the root", func(t *testing.T) {
		ar, err := sess.AcceptAnnounce("/acme/room/42/")
		require.NoError(t, err)
		defer ar.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		ann, err := ar.ReceiveAnnouncement(ctx)

		require.NoError(t, err)
		assert.Contains(t, []moqt.BroadcastPath{"/acme/room/42/host", "/acme/room/42/7"}, ann.BroadcastPath())
	})
	t.Run("lists nothing above the root", func(t *testing.T) {
		ar, err := sess.AcceptAnnounce("/acme/")
		require.NoError(t, err)
		defer ar.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		_, err = ar.ReceiveAnnouncement(ctx)

		annErr, ok := errors.AsType[*moqt.AnnounceError](err)
		require.True(t, ok, "an announce error, not %v", err)
		assert.Equal(t, moqt.AnnounceErrorCodeUnauthorized, annErr.AnnounceErrorCode())
	})
	t.Run("a withdrawn ticket grants nothing", func(t *testing.T) {
		require.NoError(t, cred.Close())

		assert.Eventually(t, func() bool {
			return isSubscribeRefused(subscribeOn(t, sess, "/acme/room/42/host"), moqt.SubscribeErrorCodeUnauthorized)
		}, 5*time.Second, 50*time.Millisecond)
	})
}
