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
		Root:      "/acme/room/42/",
		Publish:   []string{"/7"},
		Subscribe: []string{"/"},
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

	_, err := present(sign(other, token.Grant{Root: "/acme/room/42/", Subscribe: []string{"/"}}))
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

func TestRelay_AnonymousSession(t *testing.T) {
	keys, verifier := trustedKeys(t, "acme", 2)
	session, other := keys[0], keys[1]
	addr, srv := startAuthRelay(t, verifier.Authorize, nil, func(s *relay.Server) {
		s.Tickets = verifier
		s.AuthDeadline = 2 * time.Second
	})
	sign := func(key token.SigningKey, g token.Grant) string {
		tok, err := token.Sign(key, g, time.Minute)
		require.NoError(t, err)
		return tok
	}
	publishOver(t, srv, "https://"+addr+"/?jwt="+sign(session, atAndBeneath("acme/room/42/host", token.ActionPublish)),
		"/acme/room/42/host")

	t.Run("presents a ticket within its deadline", func(t *testing.T) {
		mux := moqt.NewTrackMux(0)
		sess := dialOver(t, nativeURL(addr)+"/", nil, mux)
		present := func(tok string) (*moqt.Credential, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			return sess.Auth(ctx, []byte(tok))
		}

		assert.True(t, isSubscribeRefused(subscribeOn(t, sess, "/acme/room/42/host"), moqt.SubscribeErrorCodeUnauthorized),
			"no subscription before a ticket")

		cred, err := present(sign(session, token.Grant{Root: "/acme/room/42/", Publish: []string{"/7"}, Subscribe: []string{"/"}}))
		require.NoError(t, err)
		t.Cleanup(func() { _ = cred.Close() })

		assert.NoError(t, subscribeOn(t, sess, "/acme/room/42/host"))
		end := announceOn(mux, "/acme/room/42/7")
		t.Cleanup(end)
		require.Eventually(t, routed(srv, "/acme/room/42/7"), 3*time.Second, 25*time.Millisecond)

		_, err = present(sign(other, token.Grant{Root: "/acme/room/42/", Subscribe: []string{"/"}}))
		authErr, ok := errors.AsType[*moqt.AuthError](err)
		require.True(t, ok, "a ticket of another key than the bound one is refused with AUTH_ERROR, not %v", err)
		assert.Equal(t, moqt.UnauthorizedSessionErrorCode, authErr.Code)

		select {
		case <-sess.Context().Done():
			t.Fatal("a session granted a ticket in time was closed")
		case <-time.After(3 * time.Second):
		}
	})
	t.Run("presents nothing", func(t *testing.T) {
		sess := dialOver(t, nativeURL(addr)+"/", nil, moqt.NewTrackMux(0))

		select {
		case <-sess.Context().Done():
		case <-time.After(10 * time.Second):
			t.Fatal("a session granted nothing was not closed at its deadline")
		}
	})
}

// A ticket for a root the session holds a credential for replaces that
// credential once its grant is in force, so that a renewal may narrow what
// the session may do; tickets for different roots add up.
func TestRelay_TicketRenewal(t *testing.T) {
	// Generous: a loaded runner may take seconds to apply a scope change.
	const settle = 10 * time.Second
	keys, verifier := trustedKeys(t, "acme", 1)
	key := keys[0]
	addr, srv := startAuthRelay(t, verifier.Authorize, nil, func(s *relay.Server) { s.Tickets = verifier })
	sign := func(tb testing.TB, g token.Grant) string {
		tb.Helper()
		tok, err := token.Sign(key, g, time.Minute)
		require.NoError(tb, err)
		return tok
	}
	publishOver(t, srv, "https://"+addr+"/?jwt="+sign(t, atAndBeneath("acme/room/42/host", token.ActionPublish)),
		"/acme/room/42/host")
	speak := token.Grant{Root: "/acme/room/42/", Publish: []string{"/7"}, Subscribe: []string{"/"}}
	listen := token.Grant{Root: "/acme/room/42/", Subscribe: []string{"/"}}
	// present presents tok on sess, withdrawing it when the test ends, and
	// returns the relay's answer.
	present := func(tb testing.TB, sess *moqt.Session, tok string) (*moqt.Credential, error) {
		tb.Helper()
		ctx, cancel := context.WithTimeout(tb.Context(), 5*time.Second)
		defer cancel()
		c, err := sess.Auth(ctx, []byte(tok))
		if c != nil {
			tb.Cleanup(func() { _ = c.Close() })
		}
		return c, err
	}
	// granted presents tok on sess and requires it granted.
	granted := func(tb testing.TB, sess *moqt.Session, tok string) *moqt.Credential {
		tb.Helper()
		c, err := present(tb, sess, tok)
		require.NoError(tb, err)
		return c
	}
	// dial connects with credential and returns its session and mux.
	dial := func(tb testing.TB, credential token.Grant) (*moqt.Session, *moqt.TrackMux) {
		tb.Helper()
		mux := moqt.NewTrackMux(0)
		return dialOver(t, nativeURL(addr)+"/?jwt="+sign(tb, credential), nil, mux), mux
	}
	// superseded reports whether c was withdrawn because a later ticket for
	// its root replaced it.
	superseded := func(c *moqt.Credential) func() bool {
		return func() bool {
			authErr, ok := errors.AsType[*moqt.AuthError](context.Cause(c.Context()))
			return ok && authErr.Code == moqt.NoError
		}
	}
	unrouted := func(path moqt.BroadcastPath) func() bool {
		return func() bool { return !routed(srv, path)() }
	}
	assertStaysUp := func(tb testing.TB, sess *moqt.Session) {
		tb.Helper()
		select {
		case <-sess.Context().Done():
			tb.Fatalf("the session ended: %v", context.Cause(sess.Context()))
		default:
		}
	}

	t.Run("a narrowing ticket ends the publication", func(t *testing.T) {
		sess, mux := dial(t, token.Grant{})
		old := granted(t, sess, sign(t, speak))
		end := announceOn(mux, "/acme/room/42/7")
		t.Cleanup(end)
		require.Eventually(t, routed(srv, "/acme/room/42/7"), settle, 25*time.Millisecond)

		granted(t, sess, sign(t, listen))

		assert.Eventually(t, unrouted("/acme/room/42/7"), settle, 25*time.Millisecond,
			"the announcement ends once the renewal no longer lets it publish")
		assert.Eventually(t, superseded(old), settle, 25*time.Millisecond, "the earlier ticket is withdrawn")
		assert.NoError(t, subscribeOn(t, sess, "/acme/room/42/host"), "what the renewal grants holds")
		assertStaysUp(t, sess)
	})
	t.Run("a narrowing ticket supersedes the connection's credential", func(t *testing.T) {
		sess, mux := dial(t, speak)
		end := announceOn(mux, "/acme/room/42/7")
		t.Cleanup(end)
		require.Eventually(t, routed(srv, "/acme/room/42/7"), settle, 25*time.Millisecond)

		granted(t, sess, sign(t, listen))

		assert.Eventually(t, unrouted("/acme/room/42/7"), settle, 25*time.Millisecond,
			"the announcement ends once the ticket replaced the credential that let it publish")
		_, err := present(t, sess, "")
		authErr, ok := errors.AsType[*moqt.AuthError](err)
		require.True(t, ok, "the replaced credential is refused, not %v", err)
		assert.Equal(t, moqt.UnauthorizedSessionErrorCode, authErr.Code)
		assert.NoError(t, subscribeOn(t, sess, "/acme/room/42/host"))
		assertStaysUp(t, sess)
	})
	t.Run("a renewal granting the same keeps the publication", func(t *testing.T) {
		sess, mux := dial(t, token.Grant{})
		old := granted(t, sess, sign(t, speak))
		end := announceOn(mux, "/acme/room/42/7")
		t.Cleanup(end)
		require.Eventually(t, routed(srv, "/acme/room/42/7"), settle, 25*time.Millisecond)
		before, _ := srv.TrackMux.TrackHandler("/acme/room/42/7")
		require.NotNil(t, before)

		granted(t, sess, sign(t, speak))

		require.Eventually(t, superseded(old), settle, 25*time.Millisecond, "the earlier ticket is withdrawn")
		assert.Never(t, func() bool {
			now, _ := srv.TrackMux.TrackHandler("/acme/room/42/7")
			return !before.IsActive() || now != before
		}, time.Second, 10*time.Millisecond, "the announcement is never ended or replaced")
	})
	t.Run("tickets for different roots add up", func(t *testing.T) {
		sess, mux := dial(t, token.Grant{})
		room := granted(t, sess, sign(t, speak))
		lobby := granted(t, sess, sign(t, token.Grant{Root: "/acme/lobby/", Publish: []string{"/7"}}))
		end := announceOn(mux, "/acme/room/42/7")
		t.Cleanup(end)
		endLobby := announceOn(mux, "/acme/lobby/7")
		t.Cleanup(endLobby)

		require.Eventually(t, routed(srv, "/acme/room/42/7"), settle, 25*time.Millisecond)
		require.Eventually(t, routed(srv, "/acme/lobby/7"), settle, 25*time.Millisecond)
		assert.NoError(t, room.Context().Err(), "the room's ticket stays in force")
		assert.NoError(t, lobby.Context().Err(), "the lobby's ticket stays in force")
		assert.NoError(t, subscribeOn(t, sess, "/acme/room/42/host"))
	})
}
