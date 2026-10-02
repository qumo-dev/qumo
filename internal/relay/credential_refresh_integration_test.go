//go:build integration

// Integration tests for hard expiry and in-session credential refresh
// (qumo-dev/qumo#423, qumo-deploy ADR 0035 Decision 3), on a real QUIC relay.
package relay

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/credential"
	"github.com/qumo-dev/qumo/internal/trust"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const refreshPath = moqt.BroadcastPath("/tenant/project/live")

// refreshingPublisher is a publisher whose auth track sends its first
// credential at once and each later one when refresh is called.
type refreshingPublisher struct {
	sess *moqt.Session
	next chan string
}

func (p *refreshingPublisher) refresh(token string) { p.next <- token }

func (p *refreshingPublisher) ended() bool { return p.sess.Context().Err() != nil }

// refreshTestRelay is a managed relay with a 10 ms expiry leeway and no
// refresh rate limit worth waiting for.
type refreshTestRelay struct {
	addr  string
	roots *x509.CertPool
	cp    *stubControlPlane
	poll  func()
	srv   *Server
}

func startRefreshTestRelay(t *testing.T, snapshot trust.Snapshot) *refreshTestRelay {
	t.Helper()
	return startRefreshTestRelayWith(t, snapshot, time.Millisecond)
}

func startRefreshTestRelayWith(t *testing.T, snapshot trust.Snapshot, refreshInterval time.Duration) *refreshTestRelay {
	t.Helper()
	return startManagedTestRelay(t, snapshot, relayAuth{expiryLeeway: 10 * time.Millisecond, refreshInterval: refreshInterval})
}

// startManagedTestRelay starts a managed relay trusting snapshot, with auth's
// other settings (its verifier and trust store are set here).
func startManagedTestRelay(t *testing.T, snapshot trust.Snapshot, auth relayAuth) *refreshTestRelay {
	t.Helper()
	cp := newStubControlPlane()
	t.Cleanup(cp.Close)
	cp.setSnapshot(snapshot)
	store := trust.NewStore()
	poller := trust.NewPoller(cp.srv.URL, "relay-shared-secret", cp.srv.Client(), store)
	poll := func() { require.NoError(t, poller.Poll(context.Background())) }
	poll()
	auth.verifier, auth.trust = credential.NewVerifier(store), store
	addr, srv, shutdown := startRelay(t, auth)
	t.Cleanup(shutdown)
	leaf := srv.MOQServer.TLSConfig.Certificates[0].Leaf
	require.NotNil(t, leaf)
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return &refreshTestRelay{addr: addr, roots: roots, cp: cp, poll: poll, srv: srv}
}

// setSnapshot publishes snapshot and has the relay poll it.
func (r *refreshTestRelay) setSnapshot(snapshot trust.Snapshot) {
	r.cp.setSnapshot(snapshot)
	r.poll()
}

// publish announces refreshPath with first as its credential and waits until
// the relay has admitted it.
func (r *refreshTestRelay) publish(t *testing.T, first string) *refreshingPublisher {
	t.Helper()
	dialerTLS := &tls.Config{RootCAs: r.roots, MinVersion: tls.VersionTLS13}
	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}
	mux := moqt.NewTrackMux(0)
	sess, err := (&moqt.Dialer{TLSConfig: dialerTLS, QUICConfig: quicCfg}).Dial(context.Background(), "https://"+r.addr, mux)
	require.NoError(t, err)

	p := &refreshingPublisher{sess: sess, next: make(chan string, 4)}
	p.next <- first
	broadcast := moqt.NewBroadcast()
	require.NoError(t, broadcast.Register(authTrackName, credentialTrack(p.next)))
	ann, endAnn := moqt.NewAnnouncement(context.Background(), refreshPath)
	mux.Announce(ann, broadcast)
	t.Cleanup(func() {
		endAnn()
		_ = sess.CloseWithError(moqt.NoError, "test done")
	})
	assert.Never(t, p.ended, 300*time.Millisecond, 20*time.Millisecond, "the publisher should be admitted")
	return p
}

func TestServer_CredentialExpiry_EndsSession(t *testing.T) {
	signer := newAppSigner(t)
	relay := startRefreshTestRelay(t, trust.Snapshot{Keys: []trust.SnapshotKey{signer.snapshotKey("p1", "active")}})

	p := relay.publish(t, signer.signPublish(t, "live", 1500*time.Millisecond))

	assert.Never(t, p.ended, 900*time.Millisecond, 20*time.Millisecond, "not before exp")
	require.Eventually(t, p.ended, 2*time.Second, 20*time.Millisecond, "the session ends at exp + leeway")
}

func TestServer_CredentialRefresh_ExtendsSession(t *testing.T) {
	signer := newAppSigner(t)
	relay := startRefreshTestRelay(t, trust.Snapshot{Keys: []trust.SnapshotKey{signer.snapshotKey("p1", "active")}})
	p := relay.publish(t, signer.signPublish(t, "live", 1500*time.Millisecond))

	p.refresh(signer.signPublish(t, "live", 10*time.Second))
	time.Sleep(100 * time.Millisecond)
	p.refresh(signer.signPublish(t, "live", 20*time.Second))

	assert.Never(t, p.ended, 3*time.Second, 50*time.Millisecond, "a refreshed session outlives the first credential")
}

// A refresh may be signed by another active key of the same project, and
// revocation then applies to the key of the current credential: revoking the
// old key no longer ends the session, revoking the new one does.
func TestServer_CredentialRefresh_KeyRotation(t *testing.T) {
	oldKey, newKey := newAppSigner(t), newAppSigner(t)
	relay := startRefreshTestRelay(t, trust.Snapshot{Keys: []trust.SnapshotKey{
		oldKey.snapshotKey("p1", "active"),
		newKey.snapshotKey("p1", "active"),
	}})
	p := relay.publish(t, oldKey.signPublish(t, "live", 10*time.Second))

	p.refresh(newKey.signPublish(t, "live", 10*time.Second))
	time.Sleep(200 * time.Millisecond)
	relay.setSnapshot(trust.Snapshot{Keys: []trust.SnapshotKey{
		oldKey.snapshotKey("p1", "revoked"),
		newKey.snapshotKey("p1", "active"),
	}})

	assert.Never(t, p.ended, time.Second, 20*time.Millisecond, "revoking the old key spares a session already on the new one")

	relay.setSnapshot(trust.Snapshot{Keys: []trust.SnapshotKey{newKey.snapshotKey("p1", "revoked")}})

	require.Eventually(t, p.ended, 2*time.Second, 20*time.Millisecond, "revoking the current key ends the session")
}

// Refreshes the relay must refuse; the session then ends at its first
// credential's expiry.
func TestServer_CredentialRefresh_Refused(t *testing.T) {
	signer, retired, otherProject := newAppSigner(t), newAppSigner(t), newAppSigner(t)
	snapshot := trust.Snapshot{Keys: []trust.SnapshotKey{
		signer.snapshotKey("p1", "active"),
		retired.snapshotKey("p1", "retired"),
		otherProject.snapshotKey("p2", "active"),
	}}

	cases := map[string]func(t *testing.T) string{
		"key of another project": func(t *testing.T) string { return otherProject.signPublish(t, "live", 10*time.Second) },
		"retired key":            func(t *testing.T) string { return retired.signPublish(t, "live", 10*time.Second) },
		"wider grant":            func(t *testing.T) string { return signer.signPublish(t, "", 10*time.Second) },
		"no longer covers the broadcast": func(t *testing.T) string {
			return signer.signPublish(t, "live/other", 10*time.Second)
		},
		"not a credential": func(*testing.T) string { return "garbage" },
		"oversized":        func(*testing.T) string { return strings.Repeat("a", maxCredentialBytes+1) },
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			relay := startRefreshTestRelay(t, snapshot)
			p := relay.publish(t, signer.signPublish(t, "live", 1500*time.Millisecond))

			p.refresh(token(t))

			require.Eventually(t, p.ended, 3*time.Second, 20*time.Millisecond,
				"a refused refresh leaves the first credential's expiry in force")
		})
	}
}

// Refreshes closer together than the minimum interval are dropped: the
// session keeps the first refresh's expiry, not the second's.
func TestServer_CredentialRefresh_RateLimited(t *testing.T) {
	signer := newAppSigner(t)
	relay := startRefreshTestRelayWith(t, trust.Snapshot{Keys: []trust.SnapshotKey{signer.snapshotKey("p1", "active")}}, time.Hour)
	p := relay.publish(t, signer.signPublish(t, "live", 1500*time.Millisecond))
	accepted := func() float64 { return testutil.ToFloat64(metricRefreshes.WithLabelValues(refreshAccepted)) }
	before := accepted()

	p.refresh(signer.signPublish(t, "live", 2500*time.Millisecond))
	require.Eventually(t, func() bool { return accepted() == before+1 }, 2*time.Second, 10*time.Millisecond,
		"the first refresh is accepted")
	p.refresh(signer.signPublish(t, "live", 30*time.Second))

	require.Eventually(t, p.ended, 4*time.Second, 20*time.Millisecond,
		"the second refresh, inside the interval, was dropped: the session ends at the first refresh's expiry")
	assert.Equal(t, before+1, accepted())
}
