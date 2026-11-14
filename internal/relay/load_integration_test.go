//go:build integration

package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The ceiling through real QUIC relays: tagged integration with the relay
// chain helpers it uses.

// A native-QUIC client is admitted like any client, so a full relay refuses
// it too, with the reason "full".
func TestServer_RelayPeer_NativeClientFull(t *testing.T) {
	cert, pool := chainCert(t)
	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}
	addr := chainFreeAddr(t)
	srv := &Server{
		MOQServer: &moqt.Server{Addr: addr, TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert}, NextProtos: []string{moqt.NextProtoMOQ}, MinVersion: tls.VersionTLS13,
		}, QUICConfig: quicCfg},
		MOQDialer: &moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: quicCfg},
		Config:    &Config{NodeID: "full", MaxClientSessions: 1},
		TrackMux:  moqt.NewTrackMux(moqt.NewHopID()),
		Authorize: admitUnchecked,
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
	dialer := &moqt.Dialer{TLSConfig: chainDialerTLS(pool), QUICConfig: quicCfg}
	dial := func() (*moqt.Session, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		return dialer.Dial(ctx, peerURL(addr), moqt.NewTrackMux(0))
	}
	// The listener may not be up yet: dial until a session holds.
	var held []*moqt.Session
	require.Eventually(t, func() bool {
		sess, err := dial()
		if err != nil {
			return false
		}
		held = append(held, sess)
		return true
	}, 8*time.Second, 50*time.Millisecond)
	second, err := dial()
	require.NoError(t, err)
	held = append(held, second)
	t.Cleanup(func() {
		for _, s := range held {
			assert.NoError(t, s.CloseWithError(moqt.NoError, "done"))
		}
	})
	require.Eventually(t, func() bool { return srv.load.clients.Load() == 2 }, 5*time.Second, 10*time.Millisecond)

	sess, err := dial()
	refusal := refusalOf(t, sess, err)

	assert.Equal(t, refusalFull, refusal.ErrorMessage)
	assert.Equal(t, int64(2), srv.load.clients.Load(), "the refused session took no place")
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
