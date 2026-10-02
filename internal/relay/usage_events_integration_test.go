//go:build integration

// Integration tests for usage and session events by kid and project
// (qumo-dev/qumo#424, qumo-deploy ADR 0035 Decision 6), on a real relay.
package relay

import (
	"context"

	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/trust"
	"github.com/stretchr/testify/require"
)

// eventFrom waits for the first event cp received that matches.
func eventFrom(t *testing.T, cp *stubControlPlane, what string, match func(UsageEvent) bool) UsageEvent {
	t.Helper()
	var found UsageEvent
	require.Eventually(t, func() bool {
		for _, ev := range cp.usage() {
			if match(ev) {
				found = ev
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond, "no %s event reported", what)
	return found
}

func TestServer_UsageEvents_Publisher(t *testing.T) {
	signer := newAppSigner(t)
	relay := startRefreshTestRelay(t, trust.Snapshot{Keys: []trust.SnapshotKey{signer.snapshotKey("p1", "active")}})

	p := relay.publish(t, signer.signPublish(t, "live", 1500*time.Millisecond))

	open := eventFrom(t, relay.cp, "publisher open", func(ev UsageEvent) bool {
		return ev.Type == eventSessionOpen && ev.Role == rolePublisher
	})
	require.Equal(t, signer.kid, open.KeyID)
	require.Equal(t, "p1", open.ProjectID)
	usage := eventFrom(t, relay.cp, "usage", func(ev UsageEvent) bool { return ev.Type == eventUsage })
	require.Equal(t, open.SessionID, usage.SessionID, "usage and lifecycle events share the session id")
	require.Equal(t, "p1", usage.ProjectID)

	require.Eventually(t, p.ended, 3*time.Second, 20*time.Millisecond)
	closed := eventFrom(t, relay.cp, "publisher close", func(ev UsageEvent) bool {
		return ev.Type == eventSessionClose && ev.SessionID == open.SessionID
	})
	require.Equal(t, reasonExpired, closed.Reason)
}

func TestServer_UsageEvents_KeyFollowsRefresh(t *testing.T) {
	oldKey, newKey := newAppSigner(t), newAppSigner(t)
	relay := startRefreshTestRelay(t, trust.Snapshot{Keys: []trust.SnapshotKey{
		oldKey.snapshotKey("p1", "active"),
		newKey.snapshotKey("p1", "active"),
	}})
	p := relay.publish(t, oldKey.signPublish(t, "live", time.Minute))

	p.refresh(newKey.signPublish(t, "live", time.Minute))

	eventFrom(t, relay.cp, "usage under the refreshed key", func(ev UsageEvent) bool {
		return ev.Type == eventUsage && ev.KeyID == newKey.kid && ev.ProjectID == "p1"
	})
}

func TestServer_UsageEvents_Subscriber(t *testing.T) {
	const cam = moqt.BroadcastPath("/tenant/project/live/cam1")
	publisher, viewer := newAppSigner(t), newAppSigner(t)
	relay := startRefreshTestRelay(t, trust.Snapshot{Keys: []trust.SnapshotKey{
		publisher.snapshotKey("p1", "active"),
		viewer.snapshotKey("p1", "active"),
	}})
	relay.publishMedia(t, cam, publisher.signPublish(t, "live", time.Minute))
	sub := relay.newSubscriber(t, viewer.signSubscribe(t, "live", time.Minute))
	require.NoError(t, sub.watch(t, cam))

	open := eventFrom(t, relay.cp, "subscriber open", func(ev UsageEvent) bool {
		return ev.Type == eventSessionOpen && ev.Role == roleSubscriber
	})
	require.Equal(t, viewer.kid, open.KeyID)
	require.Equal(t, "p1", open.ProjectID)

	relay.setSnapshot(trust.Snapshot{Keys: []trust.SnapshotKey{
		publisher.snapshotKey("p1", "active"),
		viewer.snapshotKey("p1", "revoked"),
	}})

	closed := eventFrom(t, relay.cp, "subscriber close", func(ev UsageEvent) bool {
		return ev.Type == eventSessionClose && ev.SessionID == open.SessionID
	})
	require.Equal(t, trust.ReasonKeyRevoked, closed.Reason)
}

// A credential admitted inside the verifier's leeway past exp expires at
// once; its session's open must still be reported before its close.
func TestServer_UsageEvents_OpenBeforeClose(t *testing.T) {
	signer := newAppSigner(t)
	relay := startRefreshTestRelay(t, trust.Snapshot{Keys: []trust.SnapshotKey{signer.snapshotKey("p1", "active")}})
	mux := moqt.NewTrackMux(0)
	next := make(chan string, 1)
	next <- signer.signPublish(t, "live", -30*time.Second)
	broadcast := moqt.NewBroadcast()
	require.NoError(t, broadcast.Register(authTrackName, credentialTrack(next)))
	ann, endAnn := moqt.NewAnnouncement(context.Background(), refreshPath)
	t.Cleanup(endAnn)
	mux.Announce(ann, broadcast)
	_ = relay.dial(t, mux)

	closed := eventFrom(t, relay.cp, "close", func(ev UsageEvent) bool { return ev.Type == eventSessionClose })

	events := relay.cp.usage()
	openAt, closeAt := -1, -1
	for i, ev := range events {
		if ev.SessionID != closed.SessionID {
			continue
		}
		switch ev.Type {
		case eventSessionOpen:
			openAt = i
		case eventSessionClose:
			closeAt = i
		}
	}
	require.NotEqual(t, -1, openAt, "the open was reported")
	require.Less(t, openAt, closeAt, "the open is reported before the close")
}

// A subscriber's close reports why it closed: credential_retracted when the
// client withdrew its session credential and stayed connected, closed when it
// disconnected.
func TestServer_UsageEvents_SubscriberCloseReason(t *testing.T) {
	const cam = moqt.BroadcastPath("/tenant/project/live/cam1")
	tests := map[string]struct {
		end  func(sess *moqt.Session, retract func())
		want string
	}{
		"retracted":    {end: func(_ *moqt.Session, retract func()) { retract() }, want: reasonRetracted},
		"disconnected": {end: func(sess *moqt.Session, _ func()) { _ = sess.CloseWithError(moqt.NoError, "bye") }, want: reasonClosed},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			signer := newAppSigner(t)
			relay := startRefreshTestRelay(t, trust.Snapshot{Keys: []trust.SnapshotKey{signer.snapshotKey("p1", "active")}})
			relay.publishMedia(t, cam, signer.signPublish(t, "live", time.Minute))
			mux := moqt.NewTrackMux(0)
			next := make(chan string, 1)
			next <- signer.signSubscribe(t, "live", time.Minute)
			broadcast := moqt.NewBroadcast()
			require.NoError(t, broadcast.Register(authTrackName, credentialTrack(next)))
			ann, retract := moqt.NewAnnouncement(context.Background(), sessionAuthPath)
			t.Cleanup(retract)
			mux.Announce(ann, broadcast)
			sub := &subscriber{sess: relay.dial(t, mux)}
			require.NoError(t, sub.watch(t, cam))
			open := eventFrom(t, relay.cp, "subscriber open", func(ev UsageEvent) bool {
				return ev.Type == eventSessionOpen && ev.Role == roleSubscriber
			})

			tt.end(sub.sess, retract)

			closed := eventFrom(t, relay.cp, "subscriber close", func(ev UsageEvent) bool {
				return ev.Type == eventSessionClose && ev.SessionID == open.SessionID
			})
			require.Equal(t, tt.want, closed.Reason)
		})
	}
}
