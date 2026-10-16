//go:build integration

// Black-box tests of the funnel: records POSTed over HTTP are committed to a
// ledger track and reach a MoQ subscriber over a real QUIC session, served by
// the funnel itself or through a relay it publishes to.
package integration

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/okdaichi/qumo-ledger/ledger/store"
	"github.com/okdaichi/qumo-ledger/ledger/store/mem"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/internal/funnel"
	"github.com/qumo-dev/qumo/internal/relay"
	"github.com/qumo-dev/qumo/token"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chatPath is the chat track of /room/123, as the funnel addresses it.
const chatPath = "/tracks/room/123/chat"

// startFunnelHTTP stands up the announce and record handler over objects,
// publishing on mux. It returns the HTTP base URL. A nil verifier accepts every
// contributor.
func startFunnelHTTP(t *testing.T, objects store.Store, mux *moqt.TrackMux, verifier *auth.Verifier) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handler, err := funnel.NewHandler(ctx, objects, mux, funnel.HandlerOptions{Verifier: verifier})
	require.NoError(t, err)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)
	return httpSrv.URL
}

// startFunnel stands up the announce and record handler and a MoQT origin
// that share one TrackMux, as funnel.Run does without a relay. It returns the
// HTTP base URL and the WebTransport URL subscribers dial.
func startFunnel(t *testing.T, objects store.Store, verifier *auth.Verifier) (ingestURL, serveURL string) {
	t.Helper()
	mux := moqt.NewTrackMux(0)
	ingestURL = startFunnelHTTP(t, objects, mux, verifier)

	certFile, keyFile := createTempCert(t)
	wtHandler := &moqt.WebTransportHandler{
		TrackMux:    mux,
		CheckOrigin: func(*http.Request) bool { return true }, // test-only permissive
		Handler: moqt.HandleFunc(func(sess *moqt.Session) {
			defer sess.CloseWithError(moqt.NoError, moqt.NoError.String())
			<-sess.Context().Done()
		}),
	}
	httpMux := http.NewServeMux()
	httpMux.Handle("/", wtHandler)
	quicAddr := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))
	moqSrv := &moqt.Server{
		Addr:               quicAddr,
		WebTransportServer: moqt.NewWebTransportServer(httpMux),
		TrackMux:           mux,
	}
	go func() { _ = moqSrv.ListenAndServeTLS(certFile, keyFile) }()
	t.Cleanup(func() {
		shutCtx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = moqSrv.Shutdown(shutCtx)
	})

	serveURL = "https://" + quicAddr + "/"
	require.Eventually(t, func() bool {
		probe := &moqt.Dialer{TLSConfig: subscriberTLS(t)}
		pctx, c := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer c()
		sess, derr := probe.Dial(pctx, serveURL, moqt.NewTrackMux(0))
		if derr != nil {
			return false
		}
		_ = sess.CloseWithError(moqt.NoError, "probe")
		return true
	}, 5*time.Second, 100*time.Millisecond, "subscriber endpoint never became reachable")

	return ingestURL, serveURL
}

// sendChat sends body to the chat track of /room/123 at the funnel with
// credential as a bearer token (none when empty), and returns the status.
func sendChat(t *testing.T, ingestURL, method, credential, body string) int {
	t.Helper()
	req, err := http.NewRequest(method, ingestURL+chatPath, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	return resp.StatusCode
}

// record posts payload to the chat track of /room/123 and returns the status.
func record(t *testing.T, ingestURL, credential, payload string) int {
	t.Helper()
	return sendChat(t, ingestURL, http.MethodPost, credential, payload)
}

// redactChat redacts the record of group in the chat track of /room/123 and
// returns the status.
func redactChat(t *testing.T, ingestURL, credential, group string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, ingestURL+chatPath+"?group="+group, nil)
	require.NoError(t, err)
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	return resp.StatusCode
}

// createChat creates the chat track of /room/123 and returns the status.
func createChat(t *testing.T, ingestURL, credential string) int {
	t.Helper()
	return sendChat(t, ingestURL, http.MethodPut, credential, "")
}

// nextRecord reads the next group of tr and returns its sequence and the
// record it carries.
func nextRecord(t *testing.T, tr *moqt.TrackReader) (moqt.GroupSequence, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gr, err := tr.AcceptGroup(ctx)
	require.NoError(t, err)
	frame := moqt.NewFrame(256)
	require.NoError(t, gr.ReadFrame(frame))
	return gr.GroupSequence(), string(frame.Body())
}

// subscribeChat subscribes to the chat track of /room/123 over url.
func subscribeChat(t *testing.T, url string) *moqt.TrackReader {
	t.Helper()
	sess := dialOver(t, url, nil, moqt.NewTrackMux(0))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, err := sess.Subscribe(ctx, "/room/123", "chat", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

// writeKeys writes a fresh signing key and the key set that trusts it, and
// returns the key, its file and the key set's file.
func writeKeys(t *testing.T) (token.SigningKey, string, string) {
	t.Helper()
	key, err := token.GenerateKey("")
	require.NoError(t, err)
	dir := t.TempDir()
	raw, err := key.MarshalJWK()
	require.NoError(t, err)
	keyFile := filepath.Join(dir, "signing-key.jwk")
	require.NoError(t, os.WriteFile(keyFile, raw, 0o600))
	set, err := token.MarshalKeySet(key.Public())
	require.NoError(t, err)
	keysFile := filepath.Join(dir, "keys.json")
	require.NoError(t, os.WriteFile(keysFile, set, 0o600))
	return key, keyFile, keysFile
}

func TestFunnel_RecordsReachAMoQSubscriber(t *testing.T) {
	ingestURL, serveURL := startFunnel(t, mem.New(), nil)
	require.Equal(t, http.StatusCreated, createChat(t, ingestURL, ""))
	require.Equal(t, http.StatusCreated, record(t, ingestURL, "", `{"user":"alice","text":"hello"}`))

	tr := subscribeChat(t, serveURL)

	// A new subscriber starts at the track's latest record, sent as it was
	// recorded.
	seq, got := nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(1), seq)
	assert.Equal(t, `{"payload":{"user":"alice","text":"hello"}}`, got)

	// Another sender records into the same track, and the subscriber receives
	// it on the one subscription.
	require.Equal(t, http.StatusCreated, record(t, ingestURL, "", `"hi"`))

	seq, got = nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(2), seq, "groups follow the ledger's commit order")
	assert.Equal(t, `{"payload":"hi"}`, got)
}

// TestFunnel_CreatedTrackWaitsForItsFirstRecord subscribes to a track created
// ahead of its first record, then records into it.
func TestFunnel_CreatedTrackWaitsForItsFirstRecord(t *testing.T) {
	ingestURL, serveURL := startFunnel(t, mem.New(), nil)
	require.Equal(t, http.StatusCreated, createChat(t, ingestURL, ""))
	require.Equal(t, http.StatusNoContent, createChat(t, ingestURL, ""), "creating it again succeeds")

	// The subscription is answered with the first record, so it waits in its
	// own goroutine.
	sess := dialOver(t, serveURL, nil, moqt.NewTrackMux(0))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type subscription struct {
		tr  *moqt.TrackReader
		err error
	}
	subscribed := make(chan subscription, 1)
	go func() {
		tr, err := sess.Subscribe(ctx, "/room/123", "chat", nil)
		subscribed <- subscription{tr: tr, err: err}
	}()
	require.Equal(t, http.StatusCreated, record(t, ingestURL, "", `"first"`))
	sub := <-subscribed
	require.NoError(t, sub.err)
	t.Cleanup(func() { _ = sub.tr.Close() })

	seq, got := nextRecord(t, sub.tr)
	assert.Equal(t, moqt.GroupSequence(1), seq)
	assert.Equal(t, `{"payload":"first"}`, got)
}

func TestFunnel_UnknownTrackIsNotFound(t *testing.T) {
	ingestURL, serveURL := startFunnel(t, mem.New(), nil)
	require.Equal(t, http.StatusCreated, createChat(t, ingestURL, ""))
	sess := dialOver(t, serveURL, nil, moqt.NewTrackMux(0))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := sess.Subscribe(ctx, "/room/123", "nosuch", nil)

	require.Error(t, err, "a track nobody created is refused, not left waiting")
	assert.NotErrorIs(t, err, context.DeadlineExceeded)
}

// TestFunnel_RestartServesRecordedTracks restarts the funnel over the store a
// previous run recorded into: subscribers reach the track before anyone
// records again, starting at its latest record, and numbering continues.
func TestFunnel_RestartServesRecordedTracks(t *testing.T) {
	objects := mem.New()
	firstURL, _ := startFunnel(t, objects, nil)
	require.Equal(t, http.StatusCreated, createChat(t, firstURL, ""))
	require.Equal(t, http.StatusCreated, record(t, firstURL, "", `"before"`))

	ingestURL, serveURL := startFunnel(t, objects, nil)
	tr := subscribeChat(t, serveURL)

	seq, got := nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(1), seq)
	assert.Equal(t, `{"payload":"before"}`, got)

	require.Equal(t, http.StatusCreated, record(t, ingestURL, "", `"after"`))
	seq, got = nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(2), seq)
	assert.Equal(t, `{"payload":"after"}`, got)
}

// signPathAuth mints a one-minute credential with a path_auth claim of pub
// and sub (each left out when empty), as other MoQ implementations sign one.
func signPathAuth(t *testing.T, key token.SigningKey, pub, sub string) string {
	t.Helper()
	pathAuth := map[string]string{"root": ""}
	if pub != "" {
		pathAuth["pub"] = pub
	}
	if sub != "" {
		pathAuth["sub"] = sub
	}
	now := time.Now().Unix()
	header, err := json.Marshal(map[string]string{"alg": "EdDSA", "kid": key.ID, "typ": "JWT"})
	require.NoError(t, err)
	claims, err := json.Marshal(map[string]any{"path_auth": pathAuth, "iat": now, "nbf": now, "exp": now + 60})
	require.NoError(t, err)
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key.Private, []byte(input)))
}

// TestFunnel_PathAuthCredentials verifies path_auth credentials, as other MoQ
// implementations sign them, against a key set as the relay does: pub, being
// publish, which sends through a relay, neither creates the track nor records,
// and sub reads history.
func TestFunnel_PathAuthCredentials(t *testing.T) {
	key, _, keysFile := writeKeys(t)
	verifier, err := auth.NewVerifier(auth.VerifierConfig{Keys: keysFile})
	require.NoError(t, err)
	ingestURL, serveURL := startFunnel(t, mem.New(), verifier)
	publisher := signPathAuth(t, key, "room/123", "")
	viewer := signPathAuth(t, key, "", "room/123")
	poster, err := token.Sign(key, exactly("room/123", token.ActionPost), time.Minute)
	require.NoError(t, err)
	creator, err := token.Sign(key, exactly("room/123", token.ActionCreate), time.Minute)
	require.NoError(t, err)

	assert.Equal(t, http.StatusUnauthorized, createChat(t, ingestURL, ""), "no credential")
	assert.Equal(t, http.StatusForbidden, createChat(t, ingestURL, viewer), "sub does not grant creating")
	assert.Equal(t, http.StatusForbidden, createChat(t, ingestURL, publisher), "pub does not grant creating")
	require.Equal(t, http.StatusCreated, createChat(t, ingestURL, creator))
	assert.Equal(t, http.StatusUnauthorized, record(t, ingestURL, "", `"unsigned"`))
	assert.Equal(t, http.StatusForbidden, record(t, ingestURL, viewer, `"a viewer"`), "subscribing does not grant recording")
	assert.Equal(t, http.StatusForbidden, record(t, ingestURL, publisher, `"a publisher"`), "pub does not grant posting")
	require.Equal(t, http.StatusCreated, record(t, ingestURL, poster, `{"type":"delete"}`))

	tr := subscribeChat(t, serveURL)
	seq, got := nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(1), seq, "only the granted record was committed")
	assert.Equal(t, `{"payload":{"type":"delete"}}`, got)

	history := func(credential string) (int, string) {
		req, err := http.NewRequest(http.MethodGet, ingestURL+chatPath, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+credential)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(body)
	}
	status, body := history(viewer)
	require.Equal(t, http.StatusOK, status)
	assert.Regexp(t, `"wallclock":\d+,"payload":\{"type":"delete"\}`, body, "a credential without sub records with no sender")
	assert.NotContains(t, body, `"sender"`)
	status, _ = history(publisher)
	assert.Equal(t, http.StatusForbidden, status, "pub does not grant reading")
}

// TestFunnel_PatternCredentialsNameSenders creates, records and reads with
// credentials whose patterns name the exact broadcast: create makes the
// track, post records into it with the credential's sub as the sender, and a
// pattern for another broadcast or action grants nothing.
func TestFunnel_PatternCredentialsNameSenders(t *testing.T) {
	key, _, keysFile := writeKeys(t)
	verifier, err := auth.NewVerifier(auth.VerifierConfig{Keys: keysFile})
	require.NoError(t, err)
	ingestURL, _ := startFunnel(t, mem.New(), verifier)
	credential := func(subject string, actions []token.Action, broadcast string) string {
		g := exactly(broadcast, actions...)
		g.Subject = subject
		c, err := token.Sign(key, g, time.Minute)
		require.NoError(t, err)
		return c
	}
	post, fetch := []token.Action{token.ActionPost}, []token.Action{token.ActionFetch}
	alice := credential("alice", post, "room/123")
	system := credential("", post, "room/123")
	reader := credential("", fetch, "room/123")
	host := credential("host", []token.Action{token.ActionCreate}, "room/123")

	assert.Equal(t, http.StatusNotFound, record(t, ingestURL, alice, `"x"`), "posting never creates the track")
	assert.Equal(t, http.StatusForbidden, createChat(t, ingestURL, alice), "posting does not grant creating")
	assert.Equal(t, http.StatusForbidden, createChat(t, ingestURL, credential("host", []token.Action{token.ActionCreate}, "room/123/alice")),
		"a broadcast beneath grants nothing above it")
	require.Equal(t, http.StatusCreated, createChat(t, ingestURL, host))
	assert.Equal(t, http.StatusForbidden, record(t, ingestURL, credential("alice", post, "room/123/alice"), `"x"`),
		"a broadcast beneath grants nothing above it")
	assert.Equal(t, http.StatusForbidden, record(t, ingestURL, reader, `"x"`), "fetching does not grant recording")
	assert.Equal(t, http.StatusForbidden, record(t, ingestURL, host, `"x"`),
		"creating the track does not grant posting")
	require.Equal(t, http.StatusCreated, record(t, ingestURL, alice, `{"name":"bob","text":"hi"}`))
	require.Equal(t, http.StatusCreated, record(t, ingestURL, system, `{"type":"delete"}`))

	history := func(credential string) (int, string) {
		req, err := http.NewRequest(http.MethodGet, ingestURL+chatPath, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+credential)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(body)
	}
	status, body := history(reader)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `"sender":"alice","payload":{"name":"bob","text":"hi"}`, "the sender is the credential's sub")
	assert.Regexp(t, `"wallclock":\d+,"payload":\{"type":"delete"\}`, body, "a credential without sub records with no sender")
	status, _ = history(alice)
	assert.Equal(t, http.StatusForbidden, status, "posting does not grant fetching")
}

// startFunnelBehindRelay runs a relay that verifies credentials and a funnel
// with no listener of its own, which dials that relay with a credential it
// signs for publishing at "room". It returns the funnel's record URL, the
// relay's address and server, and the key the relay trusts.
func startFunnelBehindRelay(t *testing.T) (ingestURL, relayAddr string, relaySrv *relay.Server, key token.SigningKey) {
	t.Helper()
	key, keyFile, keysFile := writeKeys(t)
	verifier, err := auth.NewVerifier(auth.VerifierConfig{Keys: keysFile})
	require.NoError(t, err)
	relayAddr, relaySrv = startAuthRelay(t, verifier.Authorize, nil)

	mux := moqt.NewTrackMux(0)
	ingestURL = startFunnelHTTP(t, mem.New(), mux, nil)
	ctx, cancel := context.WithCancel(context.Background())
	published := make(chan error, 1)
	go func() {
		published <- funnel.PublishThroughRelay(ctx, funnel.RelayConfig{
			URL:            nativeURL(relayAddr),
			Insecure:       true,
			SigningKeyFile: keyFile,
			Publish:        "room",
		}, mux)
	}()
	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-published, "the funnel stops publishing when its context ends")
	})
	return ingestURL, relayAddr, relaySrv, key
}

// subscribeChatAsViewer subscribes to the chat track of /room/123 through the
// relay at relayAddr, with a credential key signs for subscribing to it.
func subscribeChatAsViewer(t *testing.T, relayAddr string, key token.SigningKey) *moqt.TrackReader {
	t.Helper()
	viewer, err := token.Sign(key, atAndBeneath("room/123", token.ActionSubscribe), time.Minute)
	require.NoError(t, err)
	return subscribeChat(t, nativeURL(relayAddr)+"?jwt="+viewer)
}

// TestFunnel_PublishesThroughTheRelay runs the funnel with no listener of its
// own: it dials a relay that verifies credentials, signing a fresh one, and a
// subscriber of the relay receives what the funnel records.
func TestFunnel_PublishesThroughTheRelay(t *testing.T) {
	ingestURL, relayAddr, relaySrv, key := startFunnelBehindRelay(t)
	require.Equal(t, http.StatusCreated, createChat(t, ingestURL, ""))

	require.Equal(t, http.StatusCreated, record(t, ingestURL, "", `"first"`))
	require.Eventually(t, routed(relaySrv, "/room/123"), 5*time.Second, 25*time.Millisecond,
		"the relay routes the broadcast to the funnel")

	tr := subscribeChatAsViewer(t, relayAddr, key)

	seq, got := nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(1), seq, "the relay's subscriber starts at the latest record")
	assert.Equal(t, `{"payload":"first"}`, got)

	require.Equal(t, http.StatusCreated, record(t, ingestURL, "", `"second"`))
	seq, got = nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(2), seq)
	assert.Equal(t, `{"payload":"second"}`, got)
}

// TestFunnel_RelaySubscriberJoinsAfterSeveralRecords subscribes through the
// relay to a track that already holds several records: the subscription is
// answered with the latest of them.
func TestFunnel_RelaySubscriberJoinsAfterSeveralRecords(t *testing.T) {
	ingestURL, relayAddr, relaySrv, key := startFunnelBehindRelay(t)
	require.Equal(t, http.StatusCreated, createChat(t, ingestURL, ""))

	for _, payload := range []string{`"first"`, `"second"`, `"third"`} {
		require.Equal(t, http.StatusCreated, record(t, ingestURL, "", payload))
	}
	require.Eventually(t, routed(relaySrv, "/room/123"), 5*time.Second, 25*time.Millisecond,
		"the relay routes the broadcast to the funnel")

	tr := subscribeChatAsViewer(t, relayAddr, key)

	seq, got := nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(3), seq, "the relay's subscriber starts at the latest record")
	assert.Equal(t, `{"payload":"third"}`, got)

	require.Equal(t, http.StatusCreated, record(t, ingestURL, "", `"fourth"`))
	seq, got = nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(4), seq)
	assert.Equal(t, `{"payload":"fourth"}`, got)
}

// quietTrackWait is how long the subscriber of a track with no record waits
// before the first record arrives. It is past the 30 seconds after which an
// unanswered subscription used to be given up.
const quietTrackWait = 35 * time.Second

// TestFunnel_RelaySubscriberOutwaitsAQuietTrack subscribes through the relay
// to a track that exists and holds nothing. A publisher answers a subscription
// with its first group, so the subscription stays pending, on the subscriber's
// session and on the relay's upstream one, until the first record is recorded.
func TestFunnel_RelaySubscriberOutwaitsAQuietTrack(t *testing.T) {
	ingestURL, relayAddr, relaySrv, key := startFunnelBehindRelay(t)
	require.Equal(t, http.StatusCreated, createChat(t, ingestURL, ""))
	require.Eventually(t, routed(relaySrv, "/room/123"), 5*time.Second, 25*time.Millisecond,
		"the relay routes the broadcast to the funnel")

	viewer, err := token.Sign(key, atAndBeneath("room/123", token.ActionSubscribe), token.MaxLifetime)
	require.NoError(t, err)
	sess := dialOver(t, nativeURL(relayAddr)+"?jwt="+viewer, nil, moqt.NewTrackMux(0))
	ctx, cancel := context.WithTimeout(context.Background(), 2*quietTrackWait)
	defer cancel()
	type subscription struct {
		tr  *moqt.TrackReader
		err error
	}
	subscribed := make(chan subscription, 1)
	go func() {
		tr, err := sess.Subscribe(ctx, "/room/123", "chat", nil)
		subscribed <- subscription{tr: tr, err: err}
	}()

	select {
	case sub := <-subscribed:
		t.Fatalf("the subscription ended before the track had a record: %v", sub.err)
	case <-time.After(quietTrackWait):
	}

	require.Equal(t, http.StatusCreated, record(t, ingestURL, "", `"first"`))
	sub := <-subscribed
	require.NoError(t, sub.err)
	t.Cleanup(func() { _ = sub.tr.Close() })
	seq, got := nextRecord(t, sub.tr)
	assert.Equal(t, moqt.GroupSequence(1), seq)
	assert.Equal(t, `{"payload":"first"}`, got)
}

// TestFunnel_RedialsWhenTheRelayEndsTheSession refuses the funnel's session
// at its first revalidate: the relay ends it, the funnel dials again, and a
// subscriber that arrives afterwards still reaches the track through the
// relay.
func TestFunnel_RedialsWhenTheRelayEndsTheSession(t *testing.T) {
	var connects atomic.Int32
	// refusing refuses every revalidate until the funnel has dialed again.
	var refusing atomic.Bool
	refusing.Store(true)
	grant := testGrant(t, "**", "**", time.Second)
	relayAddr, relaySrv := startAuthRelay(t, func(_ context.Context, req auth.Request) (*auth.Grant, error) {
		switch {
		case req.Event == auth.EventConnect:
			connects.Add(1)
		case req.Event == auth.EventRevalidate && refusing.Load():
			return nil, auth.RefusedError{Status: http.StatusUnauthorized}
		}
		return grant, nil
	}, nil)
	// The relay's readiness probes connected too.
	probes := connects.Load()

	mux := moqt.NewTrackMux(0)
	ingestURL := startFunnelHTTP(t, mem.New(), mux, nil)
	ctx, cancel := context.WithCancel(context.Background())
	published := make(chan error, 1)
	go func() {
		published <- funnel.PublishThroughRelay(ctx, funnel.RelayConfig{URL: nativeURL(relayAddr), Insecure: true}, mux)
	}()
	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-published)
	})
	require.Equal(t, http.StatusCreated, createChat(t, ingestURL, ""))
	require.Equal(t, http.StatusCreated, record(t, ingestURL, "", `"kept"`))

	// The relay ends the funnel's first session; it sees the funnel dial again.
	require.Eventually(t, func() bool { return connects.Load()-probes >= 2 }, 10*time.Second, 50*time.Millisecond,
		"the funnel dials again after the relay ends its session")
	refusing.Store(false)
	require.Eventually(t, routed(relaySrv, "/room/123"), 5*time.Second, 25*time.Millisecond)

	tr := subscribeChat(t, nativeURL(relayAddr))
	seq, got := nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(1), seq)
	assert.Equal(t, `{"payload":"kept"}`, got)
}

// TestFunnel_RedactNeedsARedactPattern redacts a posted record: only a
// credential with a redact pattern may, the redaction reaches live subscribers
// carrying the credential's sub, and history keeps the record's place without
// its payload.
func TestFunnel_RedactNeedsARedactPattern(t *testing.T) {
	key, _, keysFile := writeKeys(t)
	verifier, err := auth.NewVerifier(auth.VerifierConfig{Keys: keysFile})
	require.NoError(t, err)
	ingestURL, serveURL := startFunnel(t, mem.New(), verifier)
	credential := func(subject string, action token.Action) string {
		g := exactly("room/123", action)
		g.Subject = subject
		c, err := token.Sign(key, g, time.Minute)
		require.NoError(t, err)
		return c
	}
	alice := credential("alice", token.ActionPost)
	reader := credential("", token.ActionFetch)
	moderator := credential("moderator", token.ActionRedact)
	require.Equal(t, http.StatusCreated, createChat(t, ingestURL, credential("host", token.ActionCreate)))

	require.Equal(t, http.StatusCreated, record(t, ingestURL, alice, `"hello"`))
	// A subscription starts at the track's latest record.
	tr := subscribeChat(t, serveURL)
	_, posted := nextRecord(t, tr)
	require.Equal(t, `{"sender":"alice","payload":"hello"}`, posted)
	history := func() string {
		req, err := http.NewRequest(http.MethodGet, ingestURL+chatPath, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+reader)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		return string(body)
	}
	var page struct {
		Records []struct {
			Group string `json:"group"`
		} `json:"records"`
	}
	require.NoError(t, json.Unmarshal([]byte(history()), &page))
	require.Len(t, page.Records, 1)
	group := page.Records[0].Group

	assert.Equal(t, http.StatusForbidden, redactChat(t, ingestURL, alice, group), "posting does not grant redacting")
	assert.Equal(t, http.StatusForbidden, redactChat(t, ingestURL, reader, group), "fetching does not grant redacting")
	require.Equal(t, http.StatusCreated, redactChat(t, ingestURL, moderator, group))

	_, redaction := nextRecord(t, tr)
	assert.Equal(t, `{"sender":"moderator","redacts":"`+group+`"}`, redaction, "live subscribers learn of the redaction")
	after := history()
	assert.Contains(t, after, `"group":"`+group+`"`, "the record keeps its place")
	assert.Contains(t, after, `"redacted":true`)
	assert.NotContains(t, after, `"hello"`)
}
