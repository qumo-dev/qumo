package relay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/token"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionGrant(t *testing.T) {
	g := &auth.Grant{}
	tests := map[string]struct {
		ctx  context.Context
		want *auth.Grant
	}{
		"no admission is unchecked": {ctx: context.Background(), want: nil},
		"decided with a grant":      {ctx: withAdmission(context.Background(), decidedAdmission(g, auth.Request{})), want: g},
		"decided unchecked":         {ctx: withAdmission(context.Background(), decidedAdmission(nil, auth.Request{})), want: nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := sessionGrant(tt.ctx)

			require.NoError(t, err)
			assert.Same(t, tt.want, got)
		})
	}
}

func TestSessionGrant_WaitsForPendingAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := pendingAdmission()
		ctx := withAdmission(context.Background(), a)
		g := &auth.Grant{}
		var got *auth.Grant
		var err error
		done := make(chan struct{})
		go func() {
			defer close(done)
			got, err = sessionGrant(ctx)
		}()

		synctest.Wait() // sessionGrant is blocked on the pending admission
		select {
		case <-done:
			t.Fatal("sessionGrant returned before the admission was decided")
		default:
		}
		a.decide(g, auth.Request{})
		<-done

		require.NoError(t, err)
		assert.Same(t, g, got)
	})
}

func TestSessionGrant_ContextEndsWhilePending(t *testing.T) {
	ctx, cancel := context.WithCancel(withAdmission(context.Background(), pendingAdmission()))
	cancel()

	got, err := sessionGrant(ctx)

	assert.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, got)
}

func TestServer_HandleWebTransport_Refused(t *testing.T) {
	tests := map[string]struct {
		err        error
		wantStatus int
	}{
		"refused with 401":     {err: auth.RefusedError{Status: http.StatusUnauthorized}, wantStatus: http.StatusUnauthorized},
		"refused with 403":     {err: auth.RefusedError{Status: http.StatusForbidden}, wantStatus: http.StatusForbidden},
		"can't be checked now": {err: errors.New("no key set loaded yet"), wantStatus: http.StatusServiceUnavailable},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fake := &fakeAuth{err: tt.err}
			srv := newTestServer("127.0.0.1:0")
			srv.Authorize = fake.authorize
			t.Cleanup(func() { _ = srv.Close() })
			// A WebTransport upgrade is an extended CONNECT whose :path is the
			// URL's path; httptest parses a CONNECT target as an authority, so
			// build the URL first and set the method after.
			req := httptest.NewRequest(http.MethodGet, "https://relay.example/acme/app?jwt=a.b.c", nil)
			req.Method = http.MethodConnect
			req.RemoteAddr = "192.0.2.1:5000"
			rec := httptest.NewRecorder()

			srv.HandleWebTransport(rec, req)

			assert.Equal(t, tt.wantStatus, rec.Code)
			got := fake.received()
			require.Len(t, got, 1, "an upgrade is checked once")
			assert.Equal(t, auth.EventConnect, got[0].Event)
			assert.Equal(t, auth.TransportWebTransport, got[0].Transport)
			assert.Equal(t, "/acme/app", got[0].Path)
			assert.Equal(t, "jwt=a.b.c", got[0].Query, "the credential is forwarded unparsed")
			assert.Equal(t, "192.0.2.1:5000", got[0].Remote)
			assert.Len(t, got[0].ID, 32)
		})
	}
}

func TestServer_HandleWebTransport_NotAnUpgrade(t *testing.T) {
	fake := &fakeAuth{}
	srv := newTestServer("127.0.0.1:0")
	srv.Authorize = fake.authorize
	t.Cleanup(func() { _ = srv.Close() })
	rec := httptest.NewRecorder()

	srv.HandleWebTransport(rec, httptest.NewRequest(http.MethodGet, "https://relay.example/?jwt=a.b.c", nil))

	assert.Empty(t, fake.received(), "a request that isn't an upgrade is never checked")
	assert.Equal(t, http.StatusBadRequest, rec.Code, "gomoqt answers it without a session")
}

func TestServer_HandleWebTransport_NoAuthorizeRefuses(t *testing.T) {
	srv := newTestServer("127.0.0.1:0") // authorize left unset
	t.Cleanup(func() { _ = srv.Close() })
	req := httptest.NewRequest(http.MethodGet, "https://relay.example/acme/app", nil)
	req.Method = http.MethodConnect
	rec := httptest.NewRecorder()
	counter := metricAuthRequests.WithLabelValues(auth.EventConnect, "error")

	delta := counterDelta(t, func() { srv.HandleWebTransport(rec, req) }, counter)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "a Server without authorize never runs open")
	assert.Equal(t, 1.0, delta)
}

func TestServer_Admit_Metrics(t *testing.T) {
	tests := map[string]struct {
		authorize  func(context.Context, auth.Request) (*auth.Grant, error)
		err        error
		wantResult string
	}{
		"auth off":    {authorize: admitUnchecked, wantResult: "unchecked"},
		"admitted":    {wantResult: "admitted"},
		"refused":     {err: auth.RefusedError{Status: http.StatusForbidden}, wantResult: "refused"},
		"unavailable": {err: errors.New("timeout"), wantResult: "error"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			authorize := tt.authorize
			if authorize == nil {
				authorize = (&fakeAuth{err: tt.err}).authorize
			}
			srv := &Server{Authorize: authorize}
			counter := metricAuthRequests.WithLabelValues(auth.EventConnect, tt.wantResult)
			var err error

			delta := counterDelta(t, func() {
				_, err = srv.admit(context.Background(), auth.Request{Event: auth.EventConnect})
			}, counter)

			assert.Equal(t, 1.0, delta)
			assert.ErrorIs(t, err, tt.err)
		})
	}
}

func TestServer_ReportEnd(t *testing.T) {
	stats := moqt.SessionStats{BytesSent: 9000, BytesReceived: 120}
	tests := map[string]struct {
		endErr     error
		wantResult string
	}{
		"reported":       {wantResult: authOK},
		"server failing": {endErr: errors.New("503"), wantResult: authError},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			server := &fakeAuth{endErr: tt.endErr}
			srv := &Server{End: server.end}
			connect := auth.Request{ID: "00ff", Event: auth.EventConnect, Path: "/acme", Query: "jwt=h.p.s"}

			delta := counterDelta(t, func() {
				srv.reportEnd(context.Background(), connect, stats, endDropped)
			}, metricAuthRequests.WithLabelValues(auth.EventEnd, tt.wantResult))

			assert.Equal(t, 1.0, delta)
			want := connect
			want.Event = auth.EventEnd
			want.Bytes = auth.Bytes{Sent: 9000, Received: 120}
			want.Reason = endDropped
			assert.Equal(t, []auth.Request{want}, server.ended())
		})
	}
}

// TestServer_ReportEnd_Off verifies a Server without End, as with auth off,
// reports nothing.
func TestServer_ReportEnd_Off(t *testing.T) {
	delta := counterDelta(t, func() {
		(&Server{}).reportEnd(context.Background(), auth.Request{ID: "00ff"}, moqt.SessionStats{}, endClosed)
	}, metricAuthRequests.WithLabelValues(auth.EventEnd, authOK))

	assert.Zero(t, delta)
}

// TestServer_HandleWebTransport_DisallowedOriginNotAsked verifies a request
// from an Origin the upgrade refuses is never checked, which
// would otherwise count a session that never starts.
func TestServer_HandleWebTransport_DisallowedOriginNotAsked(t *testing.T) {
	fake := &fakeAuth{grant: testGrant(t, "", "**", 0)}
	srv := newTestServer("127.0.0.1:0")
	srv.Authorize = fake.authorize
	srv.End = fake.end
	srv.AllowedOrigins = []string{"https://good.example"}
	t.Cleanup(func() { _ = srv.Close() })
	req := httptest.NewRequest(http.MethodGet, "https://relay.example/acme?jwt=a.b.c", nil)
	req.Method = http.MethodConnect
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()

	srv.HandleWebTransport(rec, req)

	assert.Empty(t, fake.received(), "a refused Origin is never checked")
	assert.Empty(t, fake.ended())
	assert.Equal(t, http.StatusBadRequest, rec.Code, "gomoqt refuses the upgrade as before")
}

// TestServer_HandleWebTransport_FailedUpgradeReportsEnd verifies an admitted
// upgrade that fails anyway reports an end, which closes the
// session it counted at connect.
func TestServer_HandleWebTransport_FailedUpgradeReportsEnd(t *testing.T) {
	fake := &fakeAuth{grant: testGrant(t, "", "**", 0)}
	srv := newTestServer("127.0.0.1:0")
	srv.Authorize = fake.authorize
	srv.End = fake.end
	t.Cleanup(func() { _ = srv.Close() })
	// An extended CONNECT without the WebTransport headers: admitted, then
	// refused by the upgrade.
	req := httptest.NewRequest(http.MethodGet, "https://relay.example/acme?jwt=a.b.c", nil)
	req.Method = http.MethodConnect
	rec := httptest.NewRecorder()

	srv.HandleWebTransport(rec, req)

	require.Len(t, fake.received(), 1)
	connect := fake.received()[0]
	want := connect
	want.Event = auth.EventEnd
	want.Reason = endUpgradeFailed
	assert.Equal(t, []auth.Request{want}, fake.ended())
}

// ticketTestKeys are a relay's trusted keys, the session's own and another of
// the same tenant, and a Verifier trusting both.
type ticketTestKeys struct {
	session, other token.SigningKey
	verifier       *auth.Verifier
}

// newTicketTestKeys returns two trusted keys and a Verifier trusting both.
func newTicketTestKeys(t *testing.T) ticketTestKeys {
	t.Helper()
	var k ticketTestKeys
	var err error
	k.session, err = token.GenerateKey("acme")
	require.NoError(t, err)
	k.other, err = token.GenerateKey("acme")
	require.NoError(t, err)
	keys := filepath.Join(t.TempDir(), "keys.json")
	writeKeyFile(t, keys, k.session.Public(), k.other.Public())
	k.verifier, err = auth.NewVerifier(auth.VerifierConfig{Keys: keys})
	require.NoError(t, err)
	return k
}

// signTest mints a credential of key granting g.
func signTest(t *testing.T, key token.SigningKey, g token.Grant) string {
	t.Helper()
	tok, err := token.Sign(key, g, credentialTTL)
	require.NoError(t, err)
	return tok
}

// dialTest dials srv over WebSocket with credential, if any, and closes the
// session when the test ends.
func dialTest(t *testing.T, srv *Server, credential string) *moqt.Session {
	t.Helper()
	target := "/acme"
	if credential != "" {
		target += "?" + url.Values{"jwt": {credential}}.Encode()
	}
	sess, err := dialWebSocket(t, srv, target)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.CloseWithError(moqt.NoError, "done") })
	return sess
}

// authTest presents tok on a new Auth Stream, withdrawing it when the test
// ends.
func authTest(t *testing.T, sess *moqt.Session, tok string) (*moqt.Credential, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := sess.Auth(ctx, []byte(tok))
	if c != nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	return c, err
}

var (
	// listenRoom subscribes beneath a room.
	listenRoom = token.Grant{Root: "acme/room/42", Subscribe: []string{"/"}}
	// listenAndSpeak subscribes beneath a room and publishes one broadcast in
	// it.
	listenAndSpeak = token.Grant{Root: "acme/room/42", Publish: []string{"/7"}, Subscribe: []string{"/"}}
)

// The credential a session connected with, presented again with no token, is
// granted what the session was admitted with: what a verified credential
// grants, nothing for one of no patterns, and everything when nothing is checked.
func TestServer_ServeAuth_ConnectionCredential(t *testing.T) {
	keys := newTicketTestKeys(t)
	tests := map[string]struct {
		credential string
		unchecked  bool
		want       moqt.Grant
	}{
		"a verified credential": {
			credential: signTest(t, keys.session, listenAndSpeak),
			want:       moqt.Grant{Root: "/acme/room/42/", Publish: []string{"/7"}, Subscribe: []string{"/"}},
		},
		"a credential of no patterns":   {credential: signTest(t, keys.session, token.Grant{}), want: moqt.Grant{}},
		"a session checked for nothing": {unchecked: true, want: everything},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var g *auth.Grant
			if !tt.unchecked {
				var err error
				g, err = keys.verifier.Authorize(t.Context(), auth.Request{
					Event: auth.EventConnect, Query: url.Values{"jwt": {tt.credential}}.Encode(),
				})
				require.NoError(t, err)
			}

			got := connectionGrant(g)

			assert.Equal(t, tt.want.Root, got.Root)
			assert.ElementsMatch(t, tt.want.Publish, got.Publish)
			assert.ElementsMatch(t, tt.want.Subscribe, got.Subscribe)
		})
	}
}

// An Auth Stream carrying no token is answered as the connection's own
// credential is, which adds nothing to the session's grants.
func TestServer_ServeAuth_EmptyTicket(t *testing.T) {
	keys := newTicketTestKeys(t)
	srv := newWebSocketTestServer(t, &fakeAuth{})
	srv.Authorize = keys.verifier.Authorize
	sess := dialTest(t, srv, signTest(t, keys.session, listenRoom))

	c, err := authTest(t, sess, "")

	require.NoError(t, err)
	assert.Equal(t, "/acme/room/42/", c.Grant().Root)
}

// A ticket is granted only to a session whose credential the relay verified,
// and only when signed by the same key; a refused one answers AUTH_ERROR.
func TestServer_ServeAuth_Ticket(t *testing.T) {
	keys := newTicketTestKeys(t)
	ticket := signTest(t, keys.session, listenAndSpeak)
	tests := map[string]struct {
		authorize  func(context.Context, auth.Request) (*auth.Grant, error)
		noTickets  bool
		credential string
		ticket     string
		wantCode   moqt.SessionErrorCode
		wantResult string
	}{
		"signed by the session's key": {
			credential: signTest(t, keys.session, token.Grant{}), ticket: ticket, wantResult: ticketGranted,
		},
		"signed by another trusted key": {
			credential: signTest(t, keys.session, token.Grant{}), ticket: signTest(t, keys.other, listenRoom),
			wantCode: moqt.UnauthorizedSessionErrorCode, wantResult: ticketRefused,
		},
		"not a credential": {
			credential: signTest(t, keys.session, token.Grant{}), ticket: "not.a.jwt",
			wantCode: moqt.UnauthorizedSessionErrorCode, wantResult: ticketRefused,
		},
		"a relay that verifies no tickets": {
			noTickets: true, credential: signTest(t, keys.session, token.Grant{}), ticket: ticket,
			wantCode: moqt.NotSupportedErrorCode, wantResult: ticketNotSupported,
		},
		"a session checked for nothing": {
			authorize: admitUnchecked, ticket: ticket,
			wantCode: moqt.NotSupportedErrorCode, wantResult: ticketNotSupported,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			srv := newWebSocketTestServer(t, &fakeAuth{})
			srv.Authorize = keys.verifier.Authorize
			if tt.authorize != nil {
				srv.Authorize = tt.authorize
			}
			if !tt.noTickets {
				srv.Tickets = keys.verifier
			}
			sess := dialTest(t, srv, tt.credential)
			counter := metricTickets.WithLabelValues(tt.wantResult)
			before := testutil.ToFloat64(counter)

			c, err := authTest(t, sess, tt.ticket)

			assert.Equal(t, before+1, testutil.ToFloat64(counter))
			if tt.wantCode != 0 {
				authErr, ok := errors.AsType[*moqt.AuthError](err)
				require.True(t, ok, "an AUTH_ERROR, not %v", err)
				assert.Equal(t, tt.wantCode, authErr.Code)
				return
			}
			require.NoError(t, err)
			g := c.Grant()
			assert.Equal(t, "/acme/room/42/", g.Root)
			assert.Equal(t, []string{"/7"}, g.Publish)
			assert.Equal(t, []string{"/"}, g.Subscribe)
		})
	}
}

// A session holds at most maxTickets at once; withdrawing one makes room.
func TestServer_ServeAuth_TooManyTickets(t *testing.T) {
	keys := newTicketTestKeys(t)
	srv := newWebSocketTestServer(t, &fakeAuth{})
	srv.Authorize = keys.verifier.Authorize
	srv.Tickets = keys.verifier
	sess := dialTest(t, srv, signTest(t, keys.session, token.Grant{}))
	ticket := signTest(t, keys.session, listenRoom)
	held := make([]*moqt.Credential, maxTickets)
	for i := range held {
		c, err := authTest(t, sess, ticket)
		require.NoError(t, err, "ticket %d", i)
		held[i] = c
	}

	_, err := authTest(t, sess, ticket)

	authErr, ok := errors.AsType[*moqt.AuthError](err)
	require.True(t, ok, "an AUTH_ERROR, not %v", err)
	assert.Equal(t, moqt.UnauthorizedSessionErrorCode, authErr.Code)
	require.NoError(t, held[0].Close())
	assert.Eventually(t, func() bool {
		_, err := authTest(t, sess, ticket)
		return err == nil
	}, 5*time.Second, 20*time.Millisecond, "a withdrawn ticket makes room for another")
}

// A granted ticket's AUTH_OK says when it lapses, so its presenter can
// present a fresh one in time.
func TestServer_ServeAuth_TicketMaxAge(t *testing.T) {
	keys := newTicketTestKeys(t)
	srv := newWebSocketTestServer(t, &fakeAuth{})
	srv.Authorize = keys.verifier.Authorize
	srv.Tickets = keys.verifier
	sess := dialTest(t, srv, signTest(t, keys.session, token.Grant{}))

	c, err := authTest(t, sess, signTest(t, keys.session, listenRoom))

	require.NoError(t, err)
	assert.Positive(t, c.Grant().MaxAge)
	assert.LessOrEqual(t, c.Grant().MaxAge, token.MaxLifetime+token.Leeway)
}

// uncheckableTickets can check no ticket just now, as a relay whose key set
// is not usable.
type uncheckableTickets struct{}

func (uncheckableTickets) Ticket(string, string) (*auth.Grant, error) {
	return nil, errors.New("auth: no key set loaded yet")
}
func (uncheckableTickets) Trusts(*auth.Grant) (bool, error) { return false, errors.New("no set") }
func (uncheckableTickets) Publishing(string)                {}

// A ticket that could not be checked is answered with INTERNAL, which a
// client may retry, not UNAUTHORIZED, which says the ticket is no good.
func TestServer_ServeAuth_TicketUncheckable(t *testing.T) {
	keys := newTicketTestKeys(t)
	srv := newWebSocketTestServer(t, &fakeAuth{})
	srv.Authorize = keys.verifier.Authorize
	srv.Tickets = uncheckableTickets{}
	sess := dialTest(t, srv, signTest(t, keys.session, token.Grant{}))

	_, err := authTest(t, sess, signTest(t, keys.session, listenRoom))

	authErr, ok := errors.AsType[*moqt.AuthError](err)
	require.True(t, ok, "want an AuthError, got %v", err)
	assert.Equal(t, moqt.InternalSessionErrorCode, authErr.Code)
}
