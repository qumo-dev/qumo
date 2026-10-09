//go:build integration

// Black-box tests of the relay's subscribe authorization (#418) and auth off,
// through its public API on a real QUIC/MOQT relay: a subscription is served
// only when the session's grant covers its path, and a refusal answers like a
// missing path.
package integration

import (
	"context"
	"crypto/tls"
	"slices"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/relay"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testTrack = moqt.TrackName("video")

// publishOver dials url and publishes each path: any track at it sends a
// one-frame group every 50 ms until its subscription ends. It returns once srv
// has routed every path.
func publishOver(t *testing.T, srv *relay.Server, url string, paths ...moqt.BroadcastPath) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mux := moqt.NewTrackMux(0)
	for _, path := range paths {
		mux.PublishFunc(ctx, path, func(tw *moqt.TrackWriter) {
			defer tw.Close()
			for {
				gw, err := tw.OpenGroup(ctx)
				if err != nil {
					return
				}
				fr := moqt.NewFrame(8)
				_, _ = fr.Write([]byte("frame"))
				_ = gw.WriteFrame(fr)
				_ = gw.Close()
				select {
				case <-tw.Context().Done():
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
		})
	}
	dialOver(t, url, nil, mux)
	for _, path := range paths {
		require.Eventually(t, routed(srv, path), 3*time.Second, 25*time.Millisecond)
	}
}

// dialOver dials url, presenting clientCert when set, and closes the session
// when the test ends.
func dialOver(t *testing.T, url string, clientCert *tls.Certificate, mux *moqt.TrackMux) *moqt.Session {
	t.Helper()
	dialerTLS := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // test-only self-signed cert
		MinVersion:         tls.VersionTLS13,
	}
	if clientCert != nil {
		dialerTLS.Certificates = []tls.Certificate{*clientCert}
	}
	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := (&moqt.Dialer{TLSConfig: dialerTLS, QUICConfig: quicCfg}).Dial(ctx, url, mux)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.CloseWithError(moqt.NoError, "test done") })
	return sess
}

// subscribe subscribes to testTrack at path over a new session to url, and
// returns nil once a group arrives, or the error that refused it.
func subscribe(t *testing.T, url string, clientCert *tls.Certificate, path moqt.BroadcastPath) error {
	t.Helper()
	sess := dialOver(t, url, clientCert, moqt.NewTrackMux(0))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, err := sess.Subscribe(ctx, path, testTrack, nil)
	if err != nil {
		return err
	}
	defer tr.Close()
	_, err = tr.AcceptGroup(ctx)
	return err
}

func TestRelay_SubscribeAuth(t *testing.T) {
	// One grant for every session: publish anywhere under acme, subscribe
	// only under acme/app.
	server := &fakeAuth{grant: testGrant(t, "acme/**", "acme/app/**", 0)}
	addr, srv := startAuthRelay(t, server.authorize, nil)
	publishOver(t, srv, "https://"+addr+"/?jwt=a.b.c", "/acme/app", "/acme/app/live", "/acme/apple/live")
	transports := map[string]string{
		"webtransport": "https://" + addr + "/?jwt=a.b.c",
		"native QUIC":  nativeURL(addr) + "/?jwt=a.b.c",
	}

	for transport, url := range transports {
		t.Run(transport+", inside the grant", func(t *testing.T) {
			err := subscribe(t, url, nil, "/acme/app/live")

			assert.NoError(t, err)
		})
		t.Run(transport+", the grant's base itself", func(t *testing.T) {
			err := subscribe(t, url, nil, "/acme/app")

			assert.NoError(t, err)
		})
		t.Run(transport+", outside the grant looks like a missing path", func(t *testing.T) {
			// acme/apple shares a string prefix with acme/app but lies outside
			// it, and it is published.
			refused := subscribe(t, url, nil, "/acme/apple/live")
			missing := subscribe(t, url, nil, "/acme/app/none")

			require.Error(t, refused)
			require.Error(t, missing)
			assert.Equal(t, missing.Error(), refused.Error())
		})
	}
}

func TestRelay_SubscribeAuth_TrustedPeerUnchecked(t *testing.T) {
	ca := newTestCA(t)
	own, peerCert := ca.issue(t, "relay-under-test", true), ca.issue(t, "relay-2", true)
	server := &fakeAuth{grant: testGrant(t, "acme/**", "acme/app/**", 0)}
	addr, srv := startAuthRelay(t, server.authorize, ca.trust(t, &own))
	publishOver(t, srv, "https://"+addr+"/?jwt=a.b.c", "/acme/apple/live")

	err := subscribe(t, nativeURL(addr)+"/", &peerCert, "/acme/apple/live")

	assert.NoError(t, err, "a relay peer subscribes outside any grant")
}

// A relay serves the SUBSCRIBEs of a peer it dialed (PEERS), which arrive over
// the session it dialed. That session never passes through its ConnContext or
// Authorize, so the subscribe check must treat it as a relay peer.
// Here relay a dials relay b, and a viewer on b watches a broadcast published
// on a: b subscribes to a over a's dialed session.
func TestRelay_SubscribeAuth_DialedPeerUnchecked(t *testing.T) {
	const path = moqt.BroadcastPath("/acme/apple/live")
	ca := newTestCA(t)
	aCert, bCert := ca.issue(t, "relay-a", true), ca.issue(t, "relay-b", true)
	bAuth := &fakeAuth{grant: testGrant(t, "acme/**", "acme/**", 0)}
	bAddr, b := startAuthRelay(t, bAuth.authorize, ca.trust(t, &bCert))
	// a's sessions may subscribe only under acme/app, which the path is not.
	aAuth := &fakeAuth{grant: testGrant(t, "acme/**", "acme/app/**", 0)}
	aAddr, a := startAuthRelay(t, aAuth.authorize, ca.trust(t, &aCert), func(s *relay.Server) {
		// a dials b with its peer certificate, asking for the peering name;
		// b answers with its own and verifies a's: each is the other's peer.
		s.Config.Peers = []relay.Peer{{Address: bAddr}}
	})
	go a.ConnectPeers(t.Context())
	publishOver(t, a, "https://"+aAddr+"/?jwt=a.b.c", path)
	require.Eventually(t, routed(b, path), 5*time.Second, 25*time.Millisecond,
		"a's broadcast should reach b over the peer link")

	err := subscribe(t, "https://"+bAddr+"/?jwt=a.b.c", nil, path)

	assert.NoError(t, err, "a serves b's SUBSCRIBE over the session a dialed, outside any grant")
}

func TestRelay_SubscribeAuth_FetchRejected(t *testing.T) {
	server := &fakeAuth{grant: testGrant(t, "acme/**", "acme/**", 0)}
	addr, srv := startAuthRelay(t, server.authorize, nil)
	publishOver(t, srv, "https://"+addr+"/?jwt=a.b.c", "/acme/app/live")
	sess := dialOver(t, nativeURL(addr)+"/?jwt=a.b.c", nil, moqt.NewTrackMux(0))

	gr, err := sess.Fetch(&moqt.FetchRequest{BroadcastPath: "/acme/app/live", TrackName: testTrack})
	if err == nil {
		// The relay registers no FetchHandler, so the stream is reset rather
		// than refused at open.
		err = gr.ReadFrame(moqt.NewFrame(64))
	}

	assert.Error(t, err)
}

func TestRelay_AuthOff(t *testing.T) {
	addr, srv := startAuthRelay(t, authOff, nil)
	publishOver(t, srv, "https://"+addr+"/", "/any/where")

	t.Run("webtransport subscribes anywhere", func(t *testing.T) {
		err := subscribe(t, "https://"+addr+"/", nil, "/any/where")

		assert.NoError(t, err)
	})
	t.Run("native QUIC subscribes anywhere", func(t *testing.T) {
		err := subscribe(t, nativeURL(addr)+"/", nil, "/any/where")

		assert.NoError(t, err)
	})
	t.Run("a credential is ignored, not refused", func(t *testing.T) {
		err := subscribe(t, "https://"+addr+"/?jwt=a.b.c", nil, "/any/where")

		assert.NoError(t, err)
	})
}

// A relay whose PEERS resolve to itself (a group name that includes it) sees
// its own certificate on the session it dialed, drops it, and does not count
// it as a peer, while its other peers are unaffected.
func TestRelay_PeersItself(t *testing.T) {
	const path = moqt.BroadcastPath("/acme/apple/live")
	ca := newTestCA(t)
	aCert, bCert := ca.issue(t, "relay-a", true), ca.issue(t, "relay-b", true)
	bAuth := &fakeAuth{grant: testGrant(t, "acme/**", "acme/**", 0)}
	bAddr, b := startAuthRelay(t, bAuth.authorize, ca.trust(t, &bCert))
	aAuth := &fakeAuth{grant: testGrant(t, "acme/**", "acme/**", 0)}
	aAddr, a := startAuthRelay(t, aAuth.authorize, ca.trust(t, &aCert))
	// As a group name resolving to both would: itself first.
	a.Config.Peers = []relay.Peer{{Address: aAddr}, {Address: bAddr}}
	go a.ConnectPeers(t.Context())
	publishOver(t, a, "https://"+aAddr+"/?jwt=a.b.c", path)

	require.Eventually(t, routed(b, path), 5*time.Second, 25*time.Millisecond, "a still peers with b")
	assert.Eventually(t, func() bool { return slices.Equal(a.ConnectedPeers(), []string{bAddr}) },
		5*time.Second, 25*time.Millisecond, "the session to itself is dropped and not counted")
	assert.Never(t, func() bool { return slices.Contains(a.ConnectedPeers(), aAddr) },
		1500*time.Millisecond, 100*time.Millisecond, "and not retried")
}
