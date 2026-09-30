//go:build integration

// Integration tests for per-project service quotas (qumo-dev/qumo#425,
// qumo-deploy ADR 0035 Decision 6), enforced per relay from the trust
// snapshot.
package relay

import (
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/trust"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServer_Quota_Broadcasts(t *testing.T) {
	one := 1
	signer := newAppSigner(t)
	relay := startRefreshTestRelay(t, trust.Snapshot{
		Keys:     []trust.SnapshotKey{signer.snapshotKey("p1", "active")},
		Policies: []trust.Policy{{ProjectID: "p1", Quotas: trust.Quotas{Broadcasts: &one}}},
	})
	routed := func(path moqt.BroadcastPath) func() bool {
		return func() bool { a, _ := relay.srv.TrackMux.TrackHandler(path); return a != nil }
	}

	relay.publishMedia(t, "/tenant/project/live/cam1", signer.signPublish(t, "live", time.Minute))
	second := relay.newPublisherAt(t, "/tenant/project/live/cam2", signer.signPublish(t, "live", time.Minute))

	assert.Never(t, routed("/tenant/project/live/cam2"), 1500*time.Millisecond, 20*time.Millisecond,
		"a broadcast past the project's quota is not routed")
	assert.False(t, second.ended(), "refusing one announcement leaves the session up")
}

func TestServer_Quota_SubscriberSessions(t *testing.T) {
	one := 1
	const cam = moqt.BroadcastPath("/tenant/project/live/cam1")
	signer := newAppSigner(t)
	relay := startRefreshTestRelay(t, trust.Snapshot{
		Keys:     []trust.SnapshotKey{signer.snapshotKey("p1", "active")},
		Policies: []trust.Policy{{ProjectID: "p1", Quotas: trust.Quotas{SubscriberSessions: &one}}},
	})
	relay.publishMedia(t, cam, signer.signPublish(t, "live", time.Minute))

	first := relay.newSubscriber(t, signer.signSubscribe(t, "live", time.Minute))
	require.NoError(t, first.watch(t, cam))
	second := relay.newSubscriber(t, signer.signSubscribe(t, "live", time.Minute))

	require.Eventually(t, second.ended, 3*time.Second, 20*time.Millisecond,
		"a subscriber session past the project's quota is closed")
	assert.False(t, first.ended())
}
