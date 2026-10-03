//go:build integration

// Integration tests for subscribe authorization (admit.go, #418) on a real
// QUIC/MOQT relay: a subscription is served only when the session's grant
// covers its path, and a refusal is indistinguishable from a missing path.
package relay

import (
	"context"
	"crypto/tls"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testTrack = moqt.TrackName("video")

// publishOver dials url and publishes each path: any track at it sends a
// one-frame group every 50 ms until its subscription ends. It returns once srv
// has routed every path.
func publishOver(t *testing.T, srv *Server, url string, paths ...moqt.BroadcastPath) {
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

func TestServer_SubscribeAuth(t *testing.T) {
	// One grant for every session: publish anywhere under acme, subscribe
	// only under acme/app.
	server := &fakeAuthServer{body: `{"publish":["acme/**"],"subscribe":["acme/app/**"]}`}
	addr, srv := startAuthRelay(t, server.start(t), nil)
	publishOver(t, srv, "https://"+addr+"/?jwt=a.b.c", "/acme/app", "/acme/app/live", "/acme/apple/live")
	transports := map[string]string{
		"webtransport": "https://" + addr + "/?jwt=a.b.c",
		"native QUIC":  peerURL(addr) + "/?jwt=a.b.c",
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

func TestServer_SubscribeAuth_TrustedPeerUnchecked(t *testing.T) {
	peerCert := loadTempCert(t)
	server := &fakeAuthServer{body: `{"publish":["acme/**"],"subscribe":["acme/app/**"]}`}
	addr, srv := startAuthRelay(t, server.start(t), &peerCert)
	publishOver(t, srv, "https://"+addr+"/?jwt=a.b.c", "/acme/apple/live")

	err := subscribe(t, peerURL(addr)+"/", &peerCert, "/acme/apple/live")

	assert.NoError(t, err, "a trusted peer subscribes outside any grant")
}

func TestServer_SubscribeAuth_FetchRejected(t *testing.T) {
	server := &fakeAuthServer{body: `{"publish":["acme/**"],"subscribe":["acme/**"]}`}
	addr, srv := startAuthRelay(t, server.start(t), nil)
	publishOver(t, srv, "https://"+addr+"/?jwt=a.b.c", "/acme/app/live")
	sess := dialOver(t, peerURL(addr)+"/?jwt=a.b.c", nil, moqt.NewTrackMux(0))

	gr, err := sess.Fetch(&moqt.FetchRequest{BroadcastPath: "/acme/app/live", TrackName: testTrack})
	if err == nil {
		// The relay registers no FetchHandler, so the stream is reset rather
		// than refused at open.
		err = gr.ReadFrame(moqt.NewFrame(64))
	}

	assert.Error(t, err)
}
