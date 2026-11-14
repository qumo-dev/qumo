//go:build integration

package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/gomoqt/transport"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The ceiling through real QUIC relays: tagged integration with the relay
// chain helpers it uses.

// refusalOf waits for sess (or the dial's err) to end, and returns how the
// relay closed it. A refusal reaches the client either while it still dials
// or just after.
func refusalOf(t *testing.T, sess *moqt.Session, err error) *transport.ApplicationError {
	t.Helper()
	var refusal *transport.ApplicationError
	if err != nil {
		require.ErrorAs(t, err, &refusal)
		return refusal
	}
	select {
	case <-sess.Context().Done():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "a refused session stayed open")
	}
	var serr *moqt.SessionError
	require.ErrorAs(t, moqt.Cause(sess.Context()), &serr)
	return serr.ApplicationError
}

// startNativeRelay starts a relay that takes native-QUIC clients, with cfg
// and authorize, and returns a dial to it that waits for the listener.
func startNativeRelay(t *testing.T, cfg *Config, authorize func(context.Context, auth.Request) (*auth.Grant, error)) (*Server, func() (*moqt.Session, error)) {
	t.Helper()
	cert, pool := chainCert(t)
	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}
	addr := chainFreeAddr(t)
	srv := &Server{
		MOQServer: &moqt.Server{Addr: addr, TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert}, NextProtos: []string{moqt.NextProtoMOQ}, MinVersion: tls.VersionTLS13,
		}, QUICConfig: quicCfg},
		MOQDialer: &moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: quicCfg},
		Config:    cfg,
		TrackMux:  moqt.NewTrackMux(moqt.NewHopID()),
		Authorize: authorize,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, moqt.ErrServerClosed) {
			t.Logf("relay stopped: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		assert.NoError(t, srv.Shutdown(ctx))
	})
	chainWaitReachable(t, addr, pool, quicCfg)
	dialer := &moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: quicCfg}
	return srv, func() (*moqt.Session, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		return dialer.Dial(ctx, peerURL(addr), moqt.NewTrackMux(0))
	}
}

// A native-QUIC client is admitted like any client, so a full relay refuses
// it too, with the reason "full"; once a session ends, it admits again.
func TestServer_RelayPeer_NativeClientFull(t *testing.T) {
	srv, dial := startNativeRelay(t, &Config{NodeID: "full", MaxClientSessions: 1}, admitUnchecked)
	// chainWaitReachable's probe session may still be ending.
	require.Eventually(t, func() bool { return srv.load.clients.Load() == 0 }, 5*time.Second, 10*time.Millisecond)
	first, err := dial()
	require.NoError(t, err)
	second, err := dial()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, second.CloseWithError(moqt.NoError, "done")) })
	require.Eventually(t, func() bool { return srv.load.clients.Load() == 2 }, 5*time.Second, 10*time.Millisecond)

	sess, err := dial()
	refusal := refusalOf(t, sess, err)

	assert.Equal(t, transport.ApplicationErrorCode(moqt.UnauthorizedSessionErrorCode), refusal.ErrorCode)
	assert.Equal(t, refusalFull, refusal.ErrorMessage)
	assert.Equal(t, int64(2), srv.load.clients.Load(), "the refused session took no place")

	require.NoError(t, first.CloseWithError(moqt.NoError, "done"))
	require.Eventually(t, func() bool { return srv.load.clients.Load() == 1 }, 5*time.Second, 10*time.Millisecond,
		"a native session that ends gives its place back")
	third, err := dial()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, third.CloseWithError(moqt.NoError, "done")) })
	assert.Eventually(t, func() bool { return srv.load.clients.Load() == 2 }, 5*time.Second, 10*time.Millisecond,
		"the session after one ended was admitted")
}

// A native-QUIC client is told why it was refused, as a WebSocket one is:
// refused (its credential), or unavailable (not checked just now).
func TestServer_RelayPeer_NativeClientRefusalReason(t *testing.T) {
	tests := map[string]struct {
		err        error
		wantReason string
	}{
		"a credential refused": {err: auth.RefusedError{Status: http.StatusForbidden}, wantReason: refusalRefused},
		"not checked just now": {err: errors.New("no key set loaded yet"), wantReason: refusalUnavailable},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fake := &fakeAuth{err: tt.err}
			srv, dial := startNativeRelay(t, &Config{NodeID: "refusing"}, fake.authorize)

			sess, err := dial()
			refusal := refusalOf(t, sess, err)

			assert.Equal(t, tt.wantReason, refusal.ErrorMessage)
			assert.Eventually(t, func() bool { return srv.load.clients.Load() == 0 }, 5*time.Second, 10*time.Millisecond,
				"a refused session gives back its place")
		})
	}
}

// What a relay delivers to its subscribers is what its egress rate is
// measured from.
func TestTrackDistributor_DeliverGroup_CountsEgress(t *testing.T) {
	const groups = 5
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	origin, leaf, pool := startRelayChain(t, ctx)
	payload := int64(len("payload")) // writeGroup's one frame
	publishToChain(t, ctx, origin, leaf, pool, func(tw *moqt.TrackWriter) {
		for seq := range moqt.GroupSequence(groups) {
			if err := writeGroup(ctx, tw, seq+1); err != nil {
				return
			}
		}
		<-tw.Context().Done()
	})
	before := leaf.sampler.egress()
	tr := subscribeToChain(t, ctx, leaf, pool, &quic.Config{EnableDatagrams: true})

	for range groups {
		gr, err := tr.AcceptGroup(ctx)
		require.NoError(t, err)
		readGroup(gr)
	}

	// The count is added once a group's frames are written, just after the
	// subscriber can read them.
	assert.Eventually(t, func() bool { return leaf.sampler.egress()-before == groups*payload }, 5*time.Second, 10*time.Millisecond,
		"egress counted %d bytes, want %d", leaf.sampler.egress()-before, groups*payload)
}
