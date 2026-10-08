package relay

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/token"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// credentialTTL is the lifetime of the credentials these tests sign.
const credentialTTL = 10 * time.Minute

// writeKeyFile writes a key set trusting keys to path, dated now: in a
// bubble, a time no earlier version of the file had once the clock moved, so
// the Verifier reads it again.
func writeKeyFile(t *testing.T, path string, keys ...token.Key) {
	t.Helper()
	raw, err := token.MarshalKeySet(keys...)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	now := time.Now()
	require.NoError(t, os.Chtimes(path, now, now))
}

// verifiedSession admits a session with a credential signed by key, carrying
// reval when reval is set, through v as the relay does, and starts its lease.
func verifiedSession(t *testing.T, v *auth.Verifier, key token.SigningKey, reval bool) (*fakeLeasedSession, *lease) {
	t.Helper()
	jwt, err := token.Sign(key, token.Grant{Scopes: []token.Scope{{Actions: []token.Action{token.ActionSubscribe, token.ActionFetch}, Broadcast: "acme/app", Prefix: true}}}, token.Options{TTL: credentialTTL, Reval: reval})
	require.NoError(t, err)
	req := auth.Request{ID: "00ff", Event: auth.EventConnect, Path: "/", Query: url.Values{"jwt": {jwt}}.Encode()}
	g, err := v.Authorize(t.Context(), req)
	require.NoError(t, err)
	sess := &fakeLeasedSession{}
	l := startLease(t.Context(), sess, v.Authorize, req, deadlineOf(g), g.Revalidate())
	require.NotNil(t, l, "every verified session is re-checked")
	return sess, l
}

// TestLease_VerifiedCredential drives a lease with the relay's Verifier: a
// credential's expiry ends its session only when the credential carries
// reval, and a withdrawn key ends every session.
func TestLease_VerifiedCredential(t *testing.T) {
	end := credentialTTL + token.Leeway
	tests := map[string]struct {
		reval bool
		// withdraw removes the key from the set once the credential has
		// expired.
		withdraw     bool
		wantAtExpiry []sessionClose
		wantClosed   []sessionClose
	}{
		"without reval, the session outlives the credential": {},
		"with reval, the session ends at its expiry": {
			reval:        true,
			wantAtExpiry: []sessionClose{{code: moqt.UnauthorizedSessionErrorCode, msg: endExpired}},
			wantClosed:   []sessionClose{{code: moqt.UnauthorizedSessionErrorCode, msg: endExpired}},
		},
		"without reval, a withdrawn key ends the session": {
			withdraw:   true,
			wantClosed: []sessionClose{{code: moqt.UnauthorizedSessionErrorCode, msg: endRefused}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				key, err := token.GenerateKey("acme")
				require.NoError(t, err)
				keys := filepath.Join(t.TempDir(), "keys.json")
				writeKeyFile(t, keys, key.Public())
				v, err := auth.NewVerifier(auth.VerifierConfig{Keys: keys})
				require.NoError(t, err)
				ctx, cancel := context.WithCancel(t.Context())
				var wg sync.WaitGroup
				wg.Go(func() { v.Run(ctx) })
				defer wg.Wait()
				defer cancel()

				sess, l := verifiedSession(t, v, key, tt.reval)
				defer l.stop()

				time.Sleep(end - time.Nanosecond)
				synctest.Wait()
				require.Empty(t, sess.closed(), "ended before the credential's expiry")
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				assert.Equal(t, tt.wantAtExpiry, sess.closed())

				if tt.withdraw {
					writeKeyFile(t, keys)
				}
				// Past the expiry, a key-set refresh and a re-check.
				time.Sleep(2 * time.Minute)
				synctest.Wait()
				assert.Equal(t, tt.wantClosed, sess.closed())
			})
		})
	}
}
