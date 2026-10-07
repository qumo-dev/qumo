//go:build integration

// Black-box tests of the funnel: records POSTed over HTTP are committed to a
// ledger track and reach a MoQ subscriber over a real QUIC session, served by
// the funnel itself or through a relay it publishes to.
package integration

import (
	"context"
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
	"github.com/okdaichi/qumo-ledger/ledger/store/memstore"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/internal/funnel"
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
	ingestURL, serveURL := startFunnel(t, memstore.New(), nil)
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
	ingestURL, serveURL := startFunnel(t, memstore.New(), nil)
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
	ingestURL, serveURL := startFunnel(t, memstore.New(), nil)
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
	objects := memstore.New()
	firstURL, _ := startFunnel(t, objects, nil)
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

// TestFunnel_CredentialsNameSendersAndReaders verifies requests against a key
// set as the relay does: a credential publishing one segment beneath the
// broadcast records as that sender, one for the broadcast records with none,
// and one subscribing at the broadcast reads its history.
func TestFunnel_CredentialsNameSendersAndReaders(t *testing.T) {
	key, _, keysFile := writeKeys(t)
	verifier, err := auth.NewVerifier(auth.VerifierConfig{Keys: keysFile})
	require.NoError(t, err)
	ingestURL, serveURL := startFunnel(t, memstore.New(), verifier)
	credential := func(g token.Grant) string {
		c, err := token.Sign(key, g, time.Minute)
		require.NoError(t, err)
		return c
	}
	alice := credential(token.Grant{Publish: "/room/123/alice"})
	system := credential(token.Grant{Publish: "/room/123"})
	viewer := credential(token.Grant{Subscribe: "/room/123"})

	assert.Equal(t, http.StatusUnauthorized, createChat(t, ingestURL, ""), "no credential")
	assert.Equal(t, http.StatusForbidden, createChat(t, ingestURL, credential(token.Grant{Publish: "/room/9"})), "another room")
	assert.Equal(t, http.StatusUnauthorized, record(t, ingestURL, "", `"unsigned"`))
	assert.Equal(t, http.StatusForbidden, record(t, ingestURL, viewer, `"a viewer"`), "subscribing does not grant recording")
	require.Equal(t, http.StatusCreated, record(t, ingestURL, alice, `{"name":"bob","text":"hi"}`))
	require.Equal(t, http.StatusCreated, record(t, ingestURL, system, `{"type":"delete"}`))

	tr := subscribeChat(t, serveURL)
	seq, got := nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(2), seq, "only the signed records were committed")
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
	assert.Contains(t, body, `"sender":"alice","payload":{"name":"bob","text":"hi"}`,
		"the sender is the credential's, whatever the payload says")
	assert.Contains(t, body, `"payload":{"type":"delete"}`)
	status, _ = history(alice)
	assert.Equal(t, http.StatusForbidden, status, "publishing does not grant reading")
}

// TestFunnel_PublishesThroughTheRelay runs the funnel with no listener of its
// own: it dials a relay that verifies credentials, signing a fresh one, and a
// subscriber of the relay receives what the funnel records.
func TestFunnel_PublishesThroughTheRelay(t *testing.T) {
	key, keyFile, keysFile := writeKeys(t)
	verifier, err := auth.NewVerifier(auth.VerifierConfig{Keys: keysFile})
	require.NoError(t, err)
	relayAddr, relaySrv := startAuthRelay(t, verifier.Authorize, nil)

	mux := moqt.NewTrackMux(0)
	ingestURL := startFunnelHTTP(t, memstore.New(), mux, nil)
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

	require.Equal(t, http.StatusCreated, record(t, ingestURL, "", `"first"`))
	require.Eventually(t, routed(relaySrv, "/room/123"), 5*time.Second, 25*time.Millisecond,
		"the relay routes the broadcast to the funnel")

	viewer, err := token.Sign(key, token.Grant{Subscribe: "room/123"}, time.Minute)
	require.NoError(t, err)
	tr := subscribeChat(t, nativeURL(relayAddr)+"?jwt="+viewer)

	seq, got := nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(1), seq, "the relay's subscriber starts at the latest record")
	assert.Equal(t, `{"payload":"first"}`, got)

	require.Equal(t, http.StatusCreated, record(t, ingestURL, "", `"second"`))
	seq, got = nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(2), seq)
	assert.Equal(t, `{"payload":"second"}`, got)
}

// TestFunnel_RedialsWhenTheRelayEndsTheSession gives each session a grant that
// expires a moment later: the relay ends the funnel's session, the funnel
// dials again, and a subscriber that arrives afterwards still reaches the
// track through the relay.
func TestFunnel_RedialsWhenTheRelayEndsTheSession(t *testing.T) {
	var connects atomic.Int32
	relayAddr, relaySrv := startAuthRelay(t, func(_ context.Context, req auth.Request) (*auth.Grant, error) {
		if req.Event == auth.EventConnect {
			connects.Add(1)
		}
		return auth.NewGrant([]string{"**"}, []string{"**"}, time.Now().Add(time.Second), 0)
	}, nil)

	mux := moqt.NewTrackMux(0)
	ingestURL := startFunnelHTTP(t, memstore.New(), mux, nil)
	ctx, cancel := context.WithCancel(context.Background())
	published := make(chan error, 1)
	go func() {
		published <- funnel.PublishThroughRelay(ctx, funnel.RelayConfig{URL: nativeURL(relayAddr), Insecure: true}, mux)
	}()
	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-published)
	})
	require.Equal(t, http.StatusCreated, record(t, ingestURL, "", `"kept"`))

	// The funnel's first session expires; the relay sees it dial again.
	require.Eventually(t, func() bool { return connects.Load() >= 2 }, 10*time.Second, 50*time.Millisecond,
		"the funnel dials again after the relay ends its session")
	require.Eventually(t, routed(relaySrv, "/room/123"), 5*time.Second, 25*time.Millisecond)

	tr := subscribeChat(t, nativeURL(relayAddr))
	seq, got := nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(1), seq)
	assert.Equal(t, `{"payload":"kept"}`, got)
}
