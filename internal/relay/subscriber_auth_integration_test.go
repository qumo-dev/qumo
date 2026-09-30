//go:build integration

// Integration tests for subscriber credentials (qumo-dev/qumo#418, qumo-deploy
// ADR 0035): an untrusted session presents a session credential on
// /.qumo/session, and each SUBSCRIBE is admitted only under its subscribe
// grant.
package relay

import (
	"context"
	"crypto/tls"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/trust"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mediaTrack serves a track that writes a small group every 50 ms until the
// subscription ends.
var mediaTrack = moqt.TrackHandlerFunc(func(tw *moqt.TrackWriter) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-tw.Context().Done():
			return
		case <-ticker.C:
		}
		g, err := tw.OpenGroup(tw.Context())
		if err != nil {
			return
		}
		f := moqt.NewFrame(4)
		_, _ = f.Write([]byte("data"))
		_ = g.WriteFrame(f)
		_ = g.Close()
	}
})

// publishMedia publishes path, with a video track, as signer's publisher.
func (r *refreshTestRelay) publishMedia(t *testing.T, path moqt.BroadcastPath, token string) {
	t.Helper()
	mux := moqt.NewTrackMux(0)
	next := make(chan string, 1)
	next <- token
	broadcast := moqt.NewBroadcast()
	require.NoError(t, broadcast.Register(authTrackName, credentialTrack(next)))
	require.NoError(t, broadcast.Register("video", mediaTrack))
	ann, endAnn := moqt.NewAnnouncement(context.Background(), path)
	mux.Announce(ann, broadcast)
	sess := r.dial(t, mux)
	t.Cleanup(func() {
		endAnn()
		_ = sess.CloseWithError(moqt.NoError, "test done")
	})
	require.Eventually(t, func() bool { a, _ := r.srv.TrackMux.TrackHandler(path); return a != nil },
		3*time.Second, 20*time.Millisecond, "the publisher's broadcast should be routed")
}

// refusedBy runs subscribe and asserts that it failed and that the relay
// refused it for result (not_covered, no_credential), so a NotFound or a
// transport error cannot pass for an authorization refusal.
func refusedBy(t *testing.T, result string, subscribe func() error) {
	t.Helper()
	before := testutil.ToFloat64(metricSubscribeAuthz.WithLabelValues(result))

	err := subscribe()

	require.Error(t, err)
	assert.Equal(t, before+1, testutil.ToFloat64(metricSubscribeAuthz.WithLabelValues(result)),
		"the relay should refuse the SUBSCRIBE as %s; got %v", result, err)
}

// subscriber is a client that only subscribes. When it has a credential, it
// presents it as its session credential and can refresh it.
type subscriber struct {
	sess *moqt.Session
	next chan string
}

func (s *subscriber) refresh(token string) { s.next <- token }

func (s *subscriber) ended() bool { return s.sess.Context().Err() != nil }

// watch subscribes to path's video track and waits for the first group.
func (s *subscriber) watch(t *testing.T, path moqt.BroadcastPath) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	reader, err := s.sess.Subscribe(ctx, path, "video", nil)
	if err != nil {
		return err
	}
	_, err = reader.AcceptGroup(ctx)
	return err
}

// newSubscriber connects a client; a non-empty token is announced as its
// session credential.
func (r *refreshTestRelay) newSubscriber(t *testing.T, token string) *subscriber {
	t.Helper()
	mux := moqt.NewTrackMux(0)
	s := &subscriber{next: make(chan string, 4)}
	if token != "" {
		s.next <- token
		broadcast := moqt.NewBroadcast()
		require.NoError(t, broadcast.Register(authTrackName, credentialTrack(s.next)))
		ann, endAnn := moqt.NewAnnouncement(context.Background(), sessionAuthPath)
		t.Cleanup(endAnn)
		mux.Announce(ann, broadcast)
	}
	s.sess = r.dial(t, mux)
	return s
}

func (r *refreshTestRelay) dial(t *testing.T, mux *moqt.TrackMux) *moqt.Session {
	t.Helper()
	dialerTLS := &tls.Config{RootCAs: r.roots, MinVersion: tls.VersionTLS13}
	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}
	sess, err := (&moqt.Dialer{TLSConfig: dialerTLS, QUICConfig: quicCfg}).Dial(context.Background(), "https://"+r.addr, mux)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.CloseWithError(moqt.NoError, "test done") })
	return sess
}

func TestServer_SubscriberCredential_Admission(t *testing.T) {
	const cam = moqt.BroadcastPath("/tenant/project/live/cam1")
	signer := newAppSigner(t)
	snapshot := trust.Snapshot{Keys: []trust.SnapshotKey{signer.snapshotKey("p1", "active")}}

	// refusal is the relay's authorization result; empty means admitted.
	cases := map[string]struct {
		token   func(t *testing.T) string
		refusal string
	}{
		"grant covers the path":     {token: func(t *testing.T) string { return signer.signSubscribe(t, "live", time.Minute) }},
		"grant is another path":     {token: func(t *testing.T) string { return signer.signSubscribe(t, "other", time.Minute) }, refusal: "not_covered"},
		"publish-only credential":   {token: func(t *testing.T) string { return signer.signPublish(t, "live", time.Minute) }, refusal: "not_covered"},
		"no session credential":     {token: func(*testing.T) string { return "" }, refusal: "no_credential"},
		"session credential forged": {token: func(t *testing.T) string { return newAppSigner(t).signSubscribe(t, "live", time.Minute) }, refusal: "no_credential"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			relay := startRefreshTestRelay(t, snapshot)
			relay.publishMedia(t, cam, signer.signPublish(t, "live", time.Minute))
			sub := relay.newSubscriber(t, tc.token(t))

			if tc.refusal != "" {
				refusedBy(t, tc.refusal, func() error { return sub.watch(t, cam) })
				return
			}
			assert.NoError(t, sub.watch(t, cam))
		})
	}
}

func TestServer_SubscriberCredential_Expiry(t *testing.T) {
	const cam = moqt.BroadcastPath("/tenant/project/live/cam1")
	signer := newAppSigner(t)
	relay := startRefreshTestRelay(t, trust.Snapshot{Keys: []trust.SnapshotKey{signer.snapshotKey("p1", "active")}})
	relay.publishMedia(t, cam, signer.signPublish(t, "live", time.Minute))
	sub := relay.newSubscriber(t, signer.signSubscribe(t, "live", 2*time.Second))
	require.NoError(t, sub.watch(t, cam))

	require.Eventually(t, sub.ended, 3*time.Second, 20*time.Millisecond, "the subscriber's session ends at exp")
}

func TestServer_SubscriberCredential_Refresh(t *testing.T) {
	const cam = moqt.BroadcastPath("/tenant/project/live/cam1")
	signer := newAppSigner(t)
	relay := startRefreshTestRelay(t, trust.Snapshot{Keys: []trust.SnapshotKey{signer.snapshotKey("p1", "active")}})
	relay.publishMedia(t, cam, signer.signPublish(t, "live", time.Minute))

	t.Run("a narrower grant that still covers the subscription extends it", func(t *testing.T) {
		sub := relay.newSubscriber(t, signer.signSubscribe(t, "live", 2*time.Second))
		require.NoError(t, sub.watch(t, cam))

		sub.refresh(signer.signSubscribe(t, "live/cam1", time.Minute))

		assert.Never(t, sub.ended, 3*time.Second, 50*time.Millisecond)
	})
	t.Run("a grant that drops an open subscription is refused", func(t *testing.T) {
		sub := relay.newSubscriber(t, signer.signSubscribe(t, "live", 2*time.Second))
		require.NoError(t, sub.watch(t, cam))

		sub.refresh(signer.signSubscribe(t, "live/cam2", time.Minute))

		require.Eventually(t, sub.ended, 3*time.Second, 20*time.Millisecond,
			"the refused refresh leaves the first expiry in force")
	})
}

func TestServer_SubscriberCredential_Revocation(t *testing.T) {
	const cam = moqt.BroadcastPath("/tenant/project/live/cam1")
	publisher, viewer := newAppSigner(t), newAppSigner(t)
	relay := startRefreshTestRelay(t, trust.Snapshot{Keys: []trust.SnapshotKey{
		publisher.snapshotKey("p1", "active"),
		viewer.snapshotKey("p1", "active"),
	}})
	relay.publishMedia(t, cam, publisher.signPublish(t, "live", time.Minute))
	sub := relay.newSubscriber(t, viewer.signSubscribe(t, "live", time.Minute))
	require.NoError(t, sub.watch(t, cam))

	relay.setSnapshot(trust.Snapshot{Keys: []trust.SnapshotKey{
		publisher.snapshotKey("p1", "active"),
		viewer.snapshotKey("p1", "revoked"),
	}})

	require.Eventually(t, sub.ended, 2*time.Second, 20*time.Millisecond, "revoking the viewer's key ends its session")
}

// Announcements under /.qumo/ are relay control: never routed, even from an
// authenticated publisher's session.
func TestServer_ReservedPathsAreNotRouted(t *testing.T) {
	signer := newAppSigner(t)
	relay := startRefreshTestRelay(t, trust.Snapshot{Keys: []trust.SnapshotKey{signer.snapshotKey("p1", "active")}})
	viewer := relay.newSubscriber(t, signer.signSubscribe(t, "", time.Minute))

	// A second client subscribing to the first one's session credential
	// broadcast must find nothing there.
	other := relay.newSubscriber(t, signer.signSubscribe(t, "", time.Minute))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := other.sess.Subscribe(ctx, sessionAuthPath, authTrackName, nil)

	assert.Error(t, err)
	_ = viewer
}

// Withdrawing the session credential announcement while staying connected
// must not leave the session authorized: the relay ends it.
func TestServer_SubscriberCredential_RetractionEndsSession(t *testing.T) {
	const cam = moqt.BroadcastPath("/tenant/project/live/cam1")
	signer := newAppSigner(t)
	relay := startRefreshTestRelay(t, trust.Snapshot{Keys: []trust.SnapshotKey{signer.snapshotKey("p1", "active")}})
	relay.publishMedia(t, cam, signer.signPublish(t, "live", time.Minute))

	mux := moqt.NewTrackMux(0)
	next := make(chan string, 1)
	next <- signer.signSubscribe(t, "live", time.Minute)
	broadcast := moqt.NewBroadcast()
	require.NoError(t, broadcast.Register(authTrackName, credentialTrack(next)))
	ann, retract := moqt.NewAnnouncement(context.Background(), sessionAuthPath)
	mux.Announce(ann, broadcast)
	sub := &subscriber{sess: relay.dial(t, mux)}
	require.NoError(t, sub.watch(t, cam))

	retract()

	require.Eventually(t, sub.ended, 3*time.Second, 20*time.Millisecond,
		"a session that withdraws its credential is ended")
}
