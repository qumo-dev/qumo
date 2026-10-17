package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/token"
	"github.com/qumo-dev/qumo/usage"
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
	listenRoom = token.Grant{Root: "/acme/room/42/", Subscribe: []string{"/"}}
	// listenAndSpeak subscribes beneath a room and publishes one broadcast in
	// it.
	listenAndSpeak = token.Grant{Root: "/acme/room/42/", Publish: []string{"/7"}, Subscribe: []string{"/"}}
)

// The credential a session connected with, presented again with no token, is
// granted what the session was admitted with, until its token expires: what
// a verified credential grants, nothing for one of no patterns, and
// everything, for good, when nothing is checked.
func TestServer_ServeAuth_ConnectionCredential(t *testing.T) {
	keys := newTicketTestKeys(t)
	tests := map[string]struct {
		credential string
		unchecked  bool
		want       moqt.Grant
		wantLapse  bool
	}{
		"a verified credential": {
			credential: signTest(t, keys.session, listenAndSpeak),
			want:       moqt.Grant{Root: "/acme/room/42/", Publish: []string{"/7"}, Subscribe: []string{"/"}},
			wantLapse:  true,
		},
		"a credential of no patterns": {credential: signTest(t, keys.session, token.Grant{}), want: moqt.Grant{}, wantLapse: true},
		// A root alone would still let the session list announcements
		// within it.
		"a root and no relay patterns": {
			credential: signTest(t, keys.session, token.Grant{Root: "/acme/room/42/", Post: []string{"/comments"}}),
			want:       moqt.Grant{}, wantLapse: true,
		},
		"a session checked for nothing": {unchecked: true, want: everything},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			srv := newWebSocketTestServer(t, &fakeAuth{})
			srv.Authorize = keys.verifier.Authorize
			if tt.unchecked {
				srv.Authorize = admitUnchecked
			}
			sess := dialTest(t, srv, tt.credential)

			c, err := authTest(t, sess, "")

			require.NoError(t, err)
			got := c.Grant()
			assert.Equal(t, tt.want.Root, got.Root)
			assert.ElementsMatch(t, tt.want.Publish, got.Publish)
			assert.ElementsMatch(t, tt.want.Subscribe, got.Subscribe)
			if !tt.wantLapse {
				assert.Zero(t, got.MaxAge, "a grant checked for nothing never lapses")
				return
			}
			assert.Positive(t, got.MaxAge, "it lapses when its token expires")
			assert.LessOrEqual(t, got.MaxAge, credentialTTL+token.Leeway)
		})
	}
}

// hold keeps a grant until its session or presenter ends it, until a margin
// after it lapses, so that gomoqt revokes it first, or until it is no longer
// trusted.
func TestHold(t *testing.T) {
	tests := map[string]struct {
		lapsesIn   time.Duration
		revalidate func() bool
		cancelIn   time.Duration
		want       heldOutcome
		wantAfter  time.Duration
	}{
		"lapses": {lapsesIn: time.Minute, want: heldLapsed, wantAfter: time.Minute + revokeMargin},
		"never lapses, ended": {
			cancelIn: time.Hour, want: heldEnded, wantAfter: time.Hour,
		},
		"revoked at a recheck": {
			lapsesIn: time.Hour, revalidate: func() bool { return false },
			want: heldRevoked, wantAfter: ticketRevalidateEvery,
		},
		"trusted, then lapses": {
			lapsesIn: time.Minute, revalidate: func() bool { return true },
			want: heldLapsed, wantAfter: time.Minute + revokeMargin,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tt.cancelIn > 0 {
					time.AfterFunc(tt.cancelIn, cancel)
				}
				var lapses time.Time
				if tt.lapsesIn > 0 {
					lapses = time.Now().Add(tt.lapsesIn)
				}
				start := time.Now()

				got := hold(ctx, lapses, tt.revalidate)

				assert.Equal(t, tt.want, got)
				assert.Equal(t, tt.wantAfter, time.Since(start))
			})
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

			// The relay counts the outcome after it answers, so the count may
			// trail the answer.
			assert.Eventually(t, func() bool { return testutil.ToFloat64(counter) == before+1 },
				5*time.Second, 10*time.Millisecond, "the outcome %q is counted once", tt.wantResult)
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

// A session holds at most maxCredentials at once, its connection's own among
// them; withdrawing one makes room.
func TestServer_ServeAuth_TooManyTickets(t *testing.T) {
	keys := newTicketTestKeys(t)
	srv := newWebSocketTestServer(t, &fakeAuth{})
	srv.Authorize = keys.verifier.Authorize
	srv.Tickets = keys.verifier
	sess := dialTest(t, srv, signTest(t, keys.session, token.Grant{}))
	ticket := signTest(t, keys.session, listenRoom)
	held := make([]*moqt.Credential, maxCredentials-1)
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

// A ticket of a root and no relay patterns grants nothing, not even listing
// announcements within its root.
func TestServer_ServeAuth_TicketOfARootAlone(t *testing.T) {
	keys := newTicketTestKeys(t)
	srv := newWebSocketTestServer(t, &fakeAuth{})
	srv.Authorize = keys.verifier.Authorize
	srv.Tickets = keys.verifier
	sess := dialTest(t, srv, signTest(t, keys.session, token.Grant{}))

	c, err := authTest(t, sess, signTest(t, keys.session, token.Grant{Root: "/acme/room/42/", Post: []string{"/comments"}}))

	require.NoError(t, err)
	assert.Empty(t, c.Grant().Root)
	assert.Empty(t, c.Grant().Publish)
	assert.Empty(t, c.Grant().Subscribe)
}

// recordingTickets verifies with the Verifier and records whether each
// ticket came from a session that had been granted what may publish.
type recordingTickets struct {
	*auth.Verifier
	mu        sync.Mutex
	published []bool
}

func (r *recordingTickets) Ticket(credential, keyID string, published bool) (*auth.Grant, error) {
	r.mu.Lock()
	r.published = append(r.published, published)
	r.mu.Unlock()
	return r.Verifier.Ticket(credential, keyID, published)
}

func (r *recordingTickets) seen() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.published)
}

// A session has published once its connection's credential, or a ticket it
// was granted, may publish; a key paused for publishing still renews its
// grant then.
func TestServer_ServeAuth_TicketKnowsAPublisher(t *testing.T) {
	keys := newTicketTestKeys(t)
	tests := map[string]struct {
		credential token.Grant
		tickets    []token.Grant
		want       []bool
	}{
		"connected to publish": {
			credential: listenAndSpeak, tickets: []token.Grant{listenRoom}, want: []bool{true},
		},
		"connected to listen, then granted a ticket to publish": {
			credential: listenRoom, tickets: []token.Grant{listenAndSpeak, listenRoom}, want: []bool{false, true},
		},
		"never published": {
			credential: token.Grant{}, tickets: []token.Grant{listenRoom, listenRoom}, want: []bool{false, false},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			tickets := &recordingTickets{Verifier: keys.verifier}
			srv := newWebSocketTestServer(t, &fakeAuth{})
			srv.Authorize = keys.verifier.Authorize
			srv.Tickets = tickets
			sess := dialTest(t, srv, signTest(t, keys.session, tt.credential))
			// The connection's own credential is granted as the session
			// starts; presenting it again waits for that.
			_, err := authTest(t, sess, "")
			require.NoError(t, err)

			for _, g := range tt.tickets {
				_, err := authTest(t, sess, signTest(t, keys.session, g))
				require.NoError(t, err)
			}

			assert.Equal(t, tt.want, tickets.seen())
		})
	}
}

// uncheckableTickets can check no ticket just now, as a relay whose key set
// is not usable.
type uncheckableTickets struct{}

func (uncheckableTickets) Ticket(string, string, bool) (*auth.Grant, error) {
	return nil, errors.New("auth: no key set loaded yet")
}
func (uncheckableTickets) Trusts(*auth.Grant) (bool, error) { return false, errors.New("no set") }
func (uncheckableTickets) FirstTicket(string) (*auth.Grant, error) {
	return nil, errors.New("auth: no key set loaded yet")
}
func (uncheckableTickets) Bind(string, string, bool) {}
func (uncheckableTickets) Publishing(string)         {}

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

// bindingTickets verifies with the Verifier and records, in order, each
// session bound to a key ("bind <kid> <publishes>") and each made a
// publisher ("publishing").
type bindingTickets struct {
	*auth.Verifier
	mu     sync.Mutex
	events []string
}

func (b *bindingTickets) Bind(id, keyID string, publishes bool) {
	b.mu.Lock()
	b.events = append(b.events, fmt.Sprintf("bind %s %t", keyID, publishes))
	b.mu.Unlock()
	b.Verifier.Bind(id, keyID, publishes)
}

func (b *bindingTickets) Publishing(id string) {
	b.mu.Lock()
	b.events = append(b.events, "publishing")
	b.mu.Unlock()
	b.Verifier.Publishing(id)
}

func (b *bindingTickets) seen() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.events)
}

// A session that connected with no credential is granted nothing for it, and
// what its tickets grant: the first ticket binds it to its key, and a ticket
// of any other trusted key is refused from then on.
func TestServer_ServeAuth_Anonymous(t *testing.T) {
	keys := newTicketTestKeys(t)
	tickets := &bindingTickets{Verifier: keys.verifier}
	srv := newWebSocketTestServer(t, &fakeAuth{})
	srv.Authorize = keys.verifier.Authorize
	srv.Tickets = tickets
	sess := dialTest(t, srv, "")

	own, err := authTest(t, sess, "")
	require.NoError(t, err)
	assert.Empty(t, own.Grant().Root, "no credential grants nothing")
	assert.Empty(t, own.Grant().Publish)
	assert.Empty(t, own.Grant().Subscribe)

	c, err := authTest(t, sess, signTest(t, keys.other, listenRoom))
	require.NoError(t, err, "the first ticket may be of any trusted key")
	assert.Equal(t, "/acme/room/42/", c.Grant().Root)
	assert.Positive(t, c.Grant().MaxAge)

	_, err = authTest(t, sess, signTest(t, keys.session, listenRoom))
	authErr, ok := errors.AsType[*moqt.AuthError](err)
	require.True(t, ok, "want an AuthError, got %v", err)
	assert.Equal(t, moqt.UnauthorizedSessionErrorCode, authErr.Code, "a ticket of another key than the bound one")

	_, err = authTest(t, sess, signTest(t, keys.other, listenAndSpeak))
	require.NoError(t, err, "a later ticket of the bound key")
	// The presenter has its answer before the relay records the grant.
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Equal(c, []string{"bind " + keys.other.ID + " false", "publishing"}, tickets.seen(),
			"bound once, to the first ticket's key, then made a publisher")
	}, time.Second, 5*time.Millisecond)
}

// Tickets granted at once on an anonymous session open its usage once, with
// whether the first granted publishes, and make it a publisher only after
// that, whichever is granted first.
func TestServer_ServeAuth_AnonymousConcurrentTickets(t *testing.T) {
	keys := newTicketTestKeys(t)
	for range 10 {
		tickets := &bindingTickets{Verifier: keys.verifier}
		srv := newWebSocketTestServer(t, &fakeAuth{})
		srv.Authorize = keys.verifier.Authorize
		srv.Tickets = tickets
		sess := dialTest(t, srv, "")
		listen, speak := signTest(t, keys.session, listenRoom), signTest(t, keys.session, listenAndSpeak)

		var wg sync.WaitGroup
		for _, tok := range []string{listen, speak} {
			wg.Go(func() {
				_, err := authTest(t, sess, tok)
				assert.NoError(t, err)
			})
		}
		wg.Wait()

		// The presenters have their answers before the relay records both
		// grants: one bind, then a publishing unless the bind published.
		var events []string
		require.Eventually(t, func() bool {
			events = tickets.seen()
			return len(events) == 2 || (len(events) == 1 && strings.HasSuffix(events[0], " true"))
		}, time.Second, 5*time.Millisecond, "events: %v", tickets.seen())
		bind := "bind " + keys.session.ID
		switch events[0] {
		case bind + " true":
			assert.Len(t, events, 1, "a session bound as a publisher is made one no second time")
		case bind + " false":
			assert.Equal(t, []string{bind + " false", "publishing"}, events)
		default:
			t.Fatalf("the first event is no bind: %v", events)
		}
	}
}

// A ticket binds an anonymous session to its key only while it is granted:
// when every ticket in flight failed and none was ever granted, the session
// is free to bind to another key; once one was granted, it is bound for
// good.
func TestAdmission_TicketBinding(t *testing.T) {
	var opened []bool
	open := func(publishes bool) { opened = append(opened, publishes) }
	publishing := func() { t.Error("no publishing expected") }
	a := pendingAdmission()

	require.True(t, a.beginTicket("k1"))
	require.True(t, a.beginTicket("k1"))
	assert.False(t, a.beginTicket("k2"), "bound tentatively to k1")
	a.settleTicket(false, false, open, publishing)
	assert.Equal(t, "k1", a.boundKey(), "another ticket of k1 is in flight")
	a.settleTicket(false, false, open, publishing)
	assert.Empty(t, a.boundKey(), "no ticket in flight and none granted")

	require.True(t, a.beginTicket("k2"))
	a.settleTicket(true, false, open, publishing)
	require.True(t, a.beginTicket("k2"))
	a.settleTicket(false, false, open, publishing)
	assert.Equal(t, "k2", a.boundKey(), "a granted ticket binds for good")
	assert.Equal(t, []bool{false}, opened)
}

// A session that connected with no credential is closed whenever it has held
// no grant that grants something for its deadline: from its start, or from
// when its tickets ended. It is reported as no_credential.
func TestServer_AnonymousDeadline(t *testing.T) {
	keys := newTicketTestKeys(t)
	newServer := func(fake *fakeAuth) *Server {
		srv := newWebSocketTestServer(t, fake)
		srv.Authorize = keys.verifier.Authorize
		srv.Tickets = keys.verifier
		srv.AuthDeadline = 300 * time.Millisecond
		return srv
	}
	closedWithin := func(sess *moqt.Session, d time.Duration) bool {
		select {
		case <-sess.Context().Done():
			return true
		case <-time.After(d):
			return false
		}
	}

	t.Run("no ticket", func(t *testing.T) {
		fake := &fakeAuth{}
		sess := dialTest(t, newServer(fake), "")

		require.True(t, closedWithin(sess, 5*time.Second), "the session was not closed at its deadline")
		require.Eventually(t, func() bool { return len(fake.ended()) == 1 }, 5*time.Second, 10*time.Millisecond)
		assert.Equal(t, usage.ReasonUnauthenticated, fake.ended()[0].Reason)
	})
	t.Run("a ticket in time", func(t *testing.T) {
		sess := dialTest(t, newServer(&fakeAuth{}), "")
		_, err := authTest(t, sess, signTest(t, keys.session, listenRoom))
		require.NoError(t, err)

		assert.False(t, closedWithin(sess, time.Second), "a session holding a ticket was closed")
	})
	t.Run("only a refused ticket", func(t *testing.T) {
		sess := dialTest(t, newServer(&fakeAuth{}), "")
		_, err := authTest(t, sess, "not.a.jwt")
		require.Error(t, err)

		assert.True(t, closedWithin(sess, 5*time.Second), "a session granted nothing was not closed at its deadline")
	})
	t.Run("a ticket that grants nothing", func(t *testing.T) {
		sess := dialTest(t, newServer(&fakeAuth{}), "")
		_, err := authTest(t, sess, signTest(t, keys.session, token.Grant{Root: "/acme/room/42/", Post: []string{"/comments"}}))
		require.NoError(t, err)

		assert.True(t, closedWithin(sess, 5*time.Second), "a session granted nothing was not closed at its deadline")
	})
	t.Run("its ticket withdrawn", func(t *testing.T) {
		sess := dialTest(t, newServer(&fakeAuth{}), "")
		c, err := authTest(t, sess, signTest(t, keys.session, listenRoom))
		require.NoError(t, err)
		require.False(t, closedWithin(sess, 600*time.Millisecond), "a session holding a ticket was closed")

		require.NoError(t, c.Close())

		assert.True(t, closedWithin(sess, 5*time.Second), "a session left with no grant was not closed at its deadline")
	})
}

// Past MaxPendingAnonymous sessions that connected with no credential and
// hold no grant yet, another is refused; one granted a ticket makes room.
func TestServer_MaxPendingAnonymous(t *testing.T) {
	keys := newTicketTestKeys(t)
	srv := newWebSocketTestServer(t, &fakeAuth{})
	srv.Authorize = keys.verifier.Authorize
	srv.Tickets = keys.verifier
	srv.MaxPendingAnonymous = 1
	stays := func(sess *moqt.Session) bool {
		select {
		case <-sess.Context().Done():
			return false
		case <-time.After(time.Second):
			return true
		}
	}

	// refused reports whether a session holding no grant is turned away. The
	// relay may refuse it while the dial is still opening it, or close it
	// just after: either is the refusal.
	refused := func() bool {
		sess, err := dialWebSocket(t, srv, "/acme")
		if err != nil {
			assert.ErrorContains(t, err, "unavailable", "refused as busy, not failed otherwise")
			return true
		}
		t.Cleanup(func() { _ = sess.CloseWithError(moqt.NoError, "done") })
		return !stays(sess)
	}

	first := dialTest(t, srv, "")
	assert.True(t, refused(), "a second session holding no grant is refused")

	_, err := authTest(t, first, signTest(t, keys.session, listenRoom))
	require.NoError(t, err)

	assert.True(t, stays(dialTest(t, srv, "")), "a session granted a ticket makes room")
	assert.True(t, stays(dialTest(t, srv, signTest(t, keys.session, token.Grant{}))),
		"a session that connected with a credential is not counted")
}
