//go:build integration

// Black-box tests of the funnel: records POSTed over HTTP are committed to a
// ledger track and reach a MoQ subscriber over a real QUIC session.
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

// startFunnel stands up the announce and record handler and a MoQT origin
// that share one TrackMux, as funnel.Run does. It returns the HTTP base URL
// and the WebTransport URL subscribers dial. Records are stored in objects. A
// nil verifier accepts every contributor.
func startFunnel(t *testing.T, objects store.Store, verifier *auth.Verifier) (ingestURL, serveURL string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	mux := moqt.NewTrackMux(0)
	handler, err := funnel.NewHandler(ctx, objects, mux, verifier)
	require.NoError(t, err)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

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

	return httpSrv.URL, serveURL
}

// postFunnel POSTs body to the funnel at path with credential as a bearer
// token (none when empty), and returns the response with its body read.
func postFunnel(t *testing.T, url, credential, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(data)
}

// announce starts a contribution of name to the chat track of /room/123 and
// returns the status and the URL records are posted to.
func announce(t *testing.T, ingestURL, credential, name string) (int, string) {
	t.Helper()
	resp, _ := postFunnel(t, ingestURL+"/announce", credential,
		`{"broadcast_path":"/room/123","track_name":"chat","name":"`+name+`"}`)
	if resp.StatusCode != http.StatusCreated {
		return resp.StatusCode, ""
	}
	location, err := resp.Location()
	require.NoError(t, err)
	return resp.StatusCode, location.String() + "/records"
}

// record posts payload to a contribution's records URL and returns the status.
func record(t *testing.T, recordsURL, credential, payload string) int {
	t.Helper()
	resp, _ := postFunnel(t, recordsURL, credential, payload)
	return resp.StatusCode
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

// subscribeChat subscribes to the chat track of /room/123.
func subscribeChat(t *testing.T, serveURL string) *moqt.TrackReader {
	t.Helper()
	sess := dialOver(t, serveURL, nil, moqt.NewTrackMux(0))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, err := sess.Subscribe(ctx, "/room/123", "chat", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func TestFunnel_RecordsReachAMoQSubscriber(t *testing.T) {
	ingestURL, serveURL := startFunnel(t, memstore.New(), nil)
	status, alice := announce(t, ingestURL, "", "alice")
	require.Equal(t, http.StatusCreated, status)
	require.Equal(t, http.StatusCreated, record(t, alice, "", `{"text":"hello"}`))

	tr := subscribeChat(t, serveURL)

	// A new subscriber starts at the track's latest record.
	seq, got := nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(1), seq)
	assert.JSONEq(t, `{"name":"alice","payload":{"text":"hello"}}`, got)

	// Another contributor records into the same track, and the subscriber
	// receives it on the one subscription.
	status, bob := announce(t, ingestURL, "", "bob")
	require.Equal(t, http.StatusCreated, status)
	require.Equal(t, http.StatusCreated, record(t, bob, "", `"hi"`))

	seq, got = nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(2), seq, "groups follow the ledger's commit order")
	assert.JSONEq(t, `{"name":"bob","payload":"hi"}`, got)
}

func TestFunnel_UnknownContributionIsRefused(t *testing.T) {
	ingestURL, _ := startFunnel(t, memstore.New(), nil)

	status := record(t, ingestURL+"/contributions/unknown/records", "", `"hello"`)

	assert.Equal(t, http.StatusNotFound, status)
}

func TestFunnel_UnknownTrackIsNotFound(t *testing.T) {
	ingestURL, serveURL := startFunnel(t, memstore.New(), nil)
	status, _ := announce(t, ingestURL, "", "alice")
	require.Equal(t, http.StatusCreated, status)
	sess := dialOver(t, serveURL, nil, moqt.NewTrackMux(0))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := sess.Subscribe(ctx, "/room/123", "nosuch", nil)

	require.Error(t, err, "a track nobody announced is refused, not left waiting")
	assert.NotErrorIs(t, err, context.DeadlineExceeded)
}

// TestFunnel_RestartServesRecordedTracks restarts the funnel over the store a
// previous run recorded into: subscribers reach the track before anyone
// announces again, starting at its latest record, and numbering continues.
func TestFunnel_RestartServesRecordedTracks(t *testing.T) {
	objects := memstore.New()
	firstURL, _ := startFunnel(t, objects, nil)
	status, alice := announce(t, firstURL, "", "alice")
	require.Equal(t, http.StatusCreated, status)
	require.Equal(t, http.StatusCreated, record(t, alice, "", `"before"`))

	ingestURL, serveURL := startFunnel(t, objects, nil)
	tr := subscribeChat(t, serveURL)

	seq, got := nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(1), seq)
	assert.JSONEq(t, `{"name":"alice","payload":"before"}`, got)

	status, alice = announce(t, ingestURL, "", "alice")
	require.Equal(t, http.StatusCreated, status)
	require.Equal(t, http.StatusCreated, record(t, alice, "", `"after"`))
	seq, got = nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(2), seq)
	assert.JSONEq(t, `{"name":"alice","payload":"after"}`, got)
}

// TestFunnel_CredentialNamesTheContributor verifies contributors against a key
// set as the relay does: a credential publishes as the name its path ends in.
func TestFunnel_CredentialNamesTheContributor(t *testing.T) {
	key, err := token.GenerateKey("")
	require.NoError(t, err)
	set, err := token.MarshalKeySet(key.Public())
	require.NoError(t, err)
	keys := filepath.Join(t.TempDir(), "keys.json")
	require.NoError(t, os.WriteFile(keys, set, 0o600))
	verifier, err := auth.NewVerifier(auth.VerifierConfig{Keys: keys})
	require.NoError(t, err)
	ingestURL, serveURL := startFunnel(t, memstore.New(), verifier)
	credential := func(path string) string {
		c, err := token.Sign(key, token.Grant{Publish: path}, time.Minute)
		require.NoError(t, err)
		return c
	}
	alice := credential("/room/123/alice")

	status, _ := announce(t, ingestURL, "", "alice")
	assert.Equal(t, http.StatusUnauthorized, status, "no credential")
	status, _ = announce(t, ingestURL, alice, "bob")
	assert.Equal(t, http.StatusForbidden, status, "alice's credential does not name bob")

	status, records := announce(t, ingestURL, alice, "alice")
	require.Equal(t, http.StatusCreated, status)
	assert.Equal(t, http.StatusUnauthorized, record(t, records, "", `"unsigned"`))
	assert.Equal(t, http.StatusForbidden, record(t, records, credential("/room/123/bob"), `"bob's"`))
	require.Equal(t, http.StatusCreated, record(t, records, alice, `"signed"`))

	tr := subscribeChat(t, serveURL)
	seq, got := nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(1), seq, "only alice's signed record was committed")
	assert.JSONEq(t, `{"name":"alice","payload":"signed"}`, got)
}
