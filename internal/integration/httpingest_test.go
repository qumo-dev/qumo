//go:build integration

// Black-box tests of the HTTP ingest: records POSTed over HTTP are committed
// to a ledger track and reach a MoQ subscriber over a real QUIC session.
package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/okdaichi/qumo-ledger/ledger/store/memstore"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/httpingest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startHTTPIngest stands up the announce and record handler and a MoQT origin
// that share one TrackMux, as httpingest.Run does. It returns the HTTP base URL
// and the WebTransport URL subscribers dial.
func startHTTPIngest(t *testing.T) (ingestURL, serveURL string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	mux := moqt.NewTrackMux(0)
	handler, err := httpingest.NewHandler(ctx, memstore.New(), mux)
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

// postIngest sends one announce or record request for the chat track of
// /room/123 and returns the status.
func postIngest(t *testing.T, ingestURL, endpoint, name, payload string) int {
	t.Helper()
	body := `{"broadcast_path":"/room/123","track_name":"chat","name":"` + name + `"`
	if payload != "" {
		body += `,"payload":` + payload
	}
	resp, err := http.Post(ingestURL+"/"+endpoint, "application/json", strings.NewReader(body+"}"))
	require.NoError(t, err)
	defer resp.Body.Close()
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

func TestHTTPIngest_RecordsReachAMoQSubscriber(t *testing.T) {
	ingestURL, serveURL := startHTTPIngest(t)
	require.Equal(t, http.StatusCreated, postIngest(t, ingestURL, "announce", "alice", ""))
	require.Equal(t, http.StatusCreated, postIngest(t, ingestURL, "record", "alice", `{"text":"hello"}`))

	sess := dialOver(t, serveURL, nil, moqt.NewTrackMux(0))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, err := sess.Subscribe(ctx, "/room/123", "chat", nil)
	require.NoError(t, err)
	defer tr.Close()

	// A new subscriber starts at the track's latest record.
	seq, record := nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(1), seq)
	assert.JSONEq(t, `{"name":"alice","payload":{"text":"hello"}}`, record)

	// Another contributor records into the same track, and the subscriber
	// receives it on the one subscription.
	require.Equal(t, http.StatusOK, postIngest(t, ingestURL, "announce", "bob", ""))
	require.Equal(t, http.StatusCreated, postIngest(t, ingestURL, "record", "bob", `"hi"`))

	seq, record = nextRecord(t, tr)
	assert.Equal(t, moqt.GroupSequence(2), seq, "groups follow the ledger's commit order")
	assert.JSONEq(t, `{"name":"bob","payload":"hi"}`, record)
}

func TestHTTPIngest_UnannouncedTrackIsRefused(t *testing.T) {
	ingestURL, _ := startHTTPIngest(t)

	status := postIngest(t, ingestURL, "record", "alice", `"hello"`)

	assert.Equal(t, http.StatusNotFound, status)
}
