//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/internal/ffpub"
	"github.com/qumo-dev/qumo/internal/ingest"
	"github.com/qumo-dev/qumo/internal/meter"
	"github.com/qumo-dev/qumo/internal/optin"
	"github.com/qumo-dev/qumo/internal/relay"
	"github.com/qumo-dev/qumo/token"
	"github.com/qumo-dev/qumo/usage"
)

// usageSink records the usage records POSTed to it.
type usageSink struct {
	mu      sync.Mutex
	records []usage.Record
}

func (s *usageSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var batch []usage.Record
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, batch...)
}

func (s *usageSink) all() []usage.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]usage.Record(nil), s.records...)
}

// TestRelay_ReportsOptInSeconds runs a paid tenant's publisher against a
// relay that reports usage: it publishes a broadcast and, on a ticket its key
// holds the ABR opt-in for, opts it into ABR. The close record of its session
// carries how long the opt-in lasted (usage.OptInSeconds), under the
// tenant's key, so a control plane can bill ABR by the time it was used.
func TestRelay_ReportsOptInSeconds(t *testing.T) {
	const src = moqt.BroadcastPath("/live/metered")
	optInPath := optin.Path(token.ABRDirectory, src)

	tenant, err := token.GenerateKey("/live/")
	require.NoError(t, err)
	tenantPub := tenant.Public()
	tenantPub.OptIns = []token.OptIn{token.OptInABR}
	set, err := token.MarshalKeySet(tenantPub)
	require.NoError(t, err)
	keys := filepath.Join(t.TempDir(), "keys.json")
	require.NoError(t, os.WriteFile(keys, set, 0o600))
	sink := &usageSink{}
	usageSrv := httptest.NewServer(sink)
	t.Cleanup(usageSrv.Close)
	verifier, err := auth.NewVerifier(auth.VerifierConfig{Keys: keys})
	require.NoError(t, err)
	runCtx, stopRun := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		verifier.Run(runCtx)
	}()
	t.Cleanup(func() { stopRun(); <-runDone })

	m, err := meter.New(meter.Config{Name: "relay", URL: usageSrv.URL})
	require.NoError(t, err)
	meterCtx, stopMeter := context.WithCancel(context.Background())
	meterDone := make(chan struct{})
	go func() {
		defer close(meterDone)
		m.Run(meterCtx)
	}()
	t.Cleanup(func() { stopMeter(); <-meterDone })

	ended := make(chan auth.Request, 4)
	addr, srv := startAuthRelay(t, verifier.Authorize, nil, func(s *relay.Server) {
		s.Tickets = verifier
		s.Meter = m
		s.End = func(ctx context.Context, req auth.Request) error {
			ended <- req
			return nil
		}
	})
	sign := func(g token.Grant) string {
		tok, err := token.Sign(tenant, g, time.Minute)
		require.NoError(t, err)
		return tok
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pubMux := moqt.NewTrackMux(0)
	pubMux.PublishFunc(ctx, src, func(tw *moqt.TrackWriter) { <-tw.Context().Done() })
	// Native QUIC: see TestABR_PaidOptIn.
	pub := dialOver(t, nativeURL(addr)+"/?jwt="+sign(token.Grant{Root: "/live/", Publish: []string{"/metered"}}), nil, pubMux)
	actx, acancel := context.WithTimeout(ctx, 3*time.Second)
	defer acancel()
	_, err = pub.Auth(actx, []byte(sign(token.Grant{Root: "/.abr/live/", Publish: []string{"/metered"}})))
	require.NoError(t, err, "the key holds ABR")
	pubMux.Publish(ctx, optInPath, optin.Handler(src))
	require.Eventually(t, routed(srv, optInPath), 5*time.Second, 25*time.Millisecond)

	// Opted in for a little over two seconds, then the publisher leaves.
	time.Sleep(2200 * time.Millisecond)
	require.NoError(t, pub.CloseWithError(moqt.NoError, "done"))
	var end auth.Request
	select {
	case end = <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay did not report the session's end")
	}
	assert.GreaterOrEqual(t, end.OptIns[token.OptInABR], 2*time.Second)
	assert.NotContains(t, end.OptIns, token.OptInHLS, "nothing was opted into HLS")

	// The meter sends what is pending once more when it stops.
	stopMeter()
	<-meterDone
	var closed *usage.Record
	for _, rec := range sink.all() {
		if rec.Type == usage.SessionClose && rec.SessionID == end.ID {
			closed = &rec
		}
	}
	require.NotNil(t, closed, "no session_close for the publisher's session in %v", sink.all())
	assert.Equal(t, tenant.ID, closed.KID)
	secs := closed.Metrics[usage.OptInSeconds("abr")]
	assert.GreaterOrEqual(t, secs, int64(2))
	assert.LessOrEqual(t, secs, int64(5), "the time the opt-in lasted, not longer")
	assert.NoError(t, closed.Validate())
}

// TestIngest_ReportsUsage pushes a stream over RTMP with two tokens, for the
// broadcast and for its ABR opt-in, to an ingest that checks them against a
// key set and reports usage. The stream's close record is under the tenant's
// key and carries the media the ingest took in and how long the opt-in
// lasted, so a control plane bills a stream pushed over RTMP as it bills one
// published over MoQ.
func TestIngest_ReportsUsage(t *testing.T) {
	if !ffpub.Available() {
		t.Skip("ffmpeg not on PATH")
	}
	tenant, err := token.GenerateKey("/live/")
	require.NoError(t, err)
	tenantPub := tenant.Public()
	tenantPub.OptIns = []token.OptIn{token.OptInABR}
	verifier := verifierFor(t, tenantPub)
	sign := func(g token.Grant) string {
		tok, err := token.Sign(tenant, g, time.Minute)
		require.NoError(t, err)
		return tok
	}
	sink := &usageSink{}
	usageSrv := httptest.NewServer(sink)
	t.Cleanup(usageSrv.Close)
	m, err := meter.New(meter.Config{Name: "rtmp", URL: usageSrv.URL})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mux := moqt.NewTrackMux(0)
	srv := ingest.NewRTMPServer(ingest.RTMPConfig{
		Addr: "127.0.0.1:0", TrackMux: mux, Authorize: verifyingAuthorizer(verifier),
		OptIns: []token.OptIn{token.OptInABR}, Meter: m,
	})
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = srv.Shutdown(context.Background())
		<-errCh
	})
	var rtmpAddr string
	require.Eventually(t, func() bool {
		if a := srv.Addr(); a != nil {
			rtmpAddr = a.String()
			return true
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "RTMP server never started listening")

	streamKey := fmt.Sprintf("metered?jwt=%s&jwt=%s",
		sign(token.Grant{Root: "/live/", Publish: []string{"/metered"}}),
		sign(token.Grant{Root: "/.abr/live/", Publish: []string{"/metered"}}))
	pub := ffpub.New(ffpub.Config{
		URL: fmt.Sprintf("rtmp://%s/live/%s", rtmpAddr, streamKey),
		GOP: 30, Width: 320, Height: 240, Framerate: 30,
	})
	pubCtx, stopPub := context.WithCancel(ctx)
	require.NoError(t, pub.Start(pubCtx))
	optInPath := optin.Path(token.ABRDirectory, "/live/metered")
	require.Eventually(t, func() bool {
		ann, _ := mux.TrackHandler(optInPath)
		return ann != nil
	}, 10*time.Second, 50*time.Millisecond, "the ingest did not opt the stream in")

	// Opted in for a little over two seconds, then the publisher stops.
	time.Sleep(2200 * time.Millisecond)
	stopPub()
	_ = pub.Wait()
	var closed *usage.Record
	require.Eventually(t, func() bool {
		require.NoError(t, m.Flush(ctx))
		for _, rec := range sink.all() {
			if rec.Type == usage.SessionClose {
				closed = &rec
			}
		}
		return closed != nil
	}, 10*time.Second, 100*time.Millisecond, "no session_close for the stream")

	assert.Equal(t, tenant.ID, closed.KID, "the tenant's key, from its token")
	assert.Positive(t, closed.Metrics[usage.IngestMediaBytes], "the media the ingest took in")
	secs := closed.Metrics[usage.OptInSeconds("abr")]
	assert.GreaterOrEqual(t, secs, int64(2))
	assert.LessOrEqual(t, secs, int64(6))
	assert.NoError(t, closed.Validate())
}
