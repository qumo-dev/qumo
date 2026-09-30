//go:build integration

// Contract tests for the gomoqt behaviour in-session credential refresh
// relies on (qumo-dev/qumo#422, qumo-deploy ADR 0035 Decision 3). They run a
// bare moqt.Server, not the relay, so a gomoqt upgrade that breaks one of
// these properties fails here, named, before it breaks the relay.
package relay

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// connTag is the ConnContext key the contract server stamps each connection
// with, to see whether per-connection state reaches a TrackWriter.
type connTag struct{}

// transportCase is one client transport: WebTransport (h3) or native QUIC.
type transportCase struct {
	name       string
	url        func(addr string) string
	nextProtos []string
}

var contractTransports = []transportCase{
	{name: "webtransport", url: func(addr string) string { return "https://" + addr }},
	{name: "native", url: peerURL, nextProtos: []string{moqt.NextProtoMOQ}},
}

// startContractServer runs a bare moqt server over both transports. Each
// accepted session is sent on the returned channel and held open until it
// ends. serverMux serves the SUBSCRIBEs clients send.
// It also returns a pool that trusts the server's self-signed certificate.
func startContractServer(t *testing.T, serverMux *moqt.TrackMux) (addr string, roots *x509.CertPool, sessions <-chan *moqt.Session) {
	t.Helper()
	certFile, keyFile := createTempCert(t)
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	require.NoError(t, err)
	require.NotNil(t, cert.Leaf)
	roots = x509.NewCertPool()
	roots.AddCert(cert.Leaf)
	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}

	ch := make(chan *moqt.Session, 4)
	handler := moqt.HandleFunc(func(sess *moqt.Session) {
		ch <- sess
		<-sess.Context().Done()
	})
	addr = fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))
	httpMux := http.NewServeMux()
	srv := &moqt.Server{
		Addr: addr,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"h3", moqt.NextProtoMOQ},
			MinVersion:   tls.VersionTLS13,
		},
		QUICConfig:         quicCfg,
		WebTransportServer: moqt.NewWebTransportServer(httpMux),
		Handler:            handler,
		TrackMux:           serverMux,
		ConnContext: func(ctx context.Context, conn moqt.StreamConn) context.Context {
			return context.WithValue(ctx, connTag{}, conn.RemoteAddr().String())
		},
	}
	wt := &moqt.WebTransportHandler{TrackMux: serverMux, Handler: handler}
	httpMux.Handle("/", wt)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	dialerTLS := &tls.Config{NextProtos: []string{moqt.NextProtoMOQ}, RootCAs: roots, MinVersion: tls.VersionTLS13}
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		sess, derr := (&moqt.Dialer{TLSConfig: dialerTLS, QUICConfig: quicCfg}).Dial(ctx, peerURL(addr), moqt.NewTrackMux(0))
		if derr != nil {
			return false
		}
		_ = sess.CloseWithError(0, "probe")
		return true
	}, 5*time.Second, 50*time.Millisecond, "contract server never became reachable")
	// Drain the probe sessions.
	drained := make(chan *moqt.Session, 4)
	go func() {
		for sess := range ch {
			if sess.Context().Err() == nil {
				drained <- sess
			}
		}
	}()
	return addr, roots, drained
}

// dialContract connects a client with clientMux over tc.
func dialContract(t *testing.T, tc transportCase, addr string, roots *x509.CertPool, clientMux *moqt.TrackMux) *moqt.Session {
	t.Helper()
	dialerTLS := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13, NextProtos: tc.nextProtos}
	quicCfg := &quic.Config{EnableDatagrams: true, KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 30 * time.Second}
	sess, err := (&moqt.Dialer{TLSConfig: dialerTLS, QUICConfig: quicCfg}).Dial(context.Background(), tc.url(addr), clientMux)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.CloseWithError(moqt.NoError, "test done") })
	return sess
}

// acceptServerSession waits for the server side of the client's session.
func acceptServerSession(t *testing.T, sessions <-chan *moqt.Session) *moqt.Session {
	t.Helper()
	select {
	case sess := <-sessions:
		return sess
	case <-time.After(5 * time.Second):
		t.Fatal("server never accepted the session")
		return nil
	}
}

// readGroup reads one group from reader and returns its concatenated payload.
func readGroup(ctx context.Context, t *testing.T, reader *moqt.TrackReader) (string, error) {
	t.Helper()
	gr, err := reader.AcceptGroup(ctx)
	if err != nil {
		return "", err
	}
	var out []byte
	for f := range gr.Frames(moqt.NewFrame(0)) {
		out = append(out, f.Body()...)
	}
	return string(out), nil
}

// credentialTrack serves an "auth" track that writes each value received on
// next as its own group, until next is closed.
func credentialTrack(next <-chan string) moqt.TrackHandler {
	return moqt.TrackHandlerFunc(func(tw *moqt.TrackWriter) {
		for {
			var token string
			select {
			case <-tw.Context().Done():
				return
			case token = <-next:
			}
			g, err := tw.OpenGroup(tw.Context())
			if err != nil {
				return
			}
			f := moqt.NewFrame(len(token))
			_, _ = f.Write([]byte(token))
			_ = g.WriteFrame(f)
			_ = g.Close()
		}
	})
}

// Q1: a subscription to the auth track stays open for the session's life, and
// each later group (a refreshed credential) is delivered promptly on it.
func TestContract_AuthTrackStaysOpenForRefresh(t *testing.T) {
	for _, tc := range contractTransports {
		t.Run(tc.name, func(t *testing.T) {
			addr, roots, sessions := startContractServer(t, moqt.NewTrackMux(0))
			clientMux := moqt.NewTrackMux(0)
			next := make(chan string, 1)
			broadcast := moqt.NewBroadcast()
			require.NoError(t, broadcast.Register(authTrackName, credentialTrack(next)))
			ann, endAnn := moqt.NewAnnouncement(context.Background(), "/pub")
			t.Cleanup(endAnn)
			clientMux.Announce(ann, broadcast)
			_ = dialContract(t, tc, addr, roots, clientMux)
			server := acceptServerSession(t, sessions)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			announced, err := server.AcceptAnnounce("/")
			require.NoError(t, err)
			serverAnn, err := announced.ReceiveAnnouncement(ctx)
			require.NoError(t, err)

			// gomoqt answers a SUBSCRIBE (SUBSCRIBE_OK carries the resolved
			// start group) only once the publisher opens its first group, so
			// the first credential must already be on its way.
			next <- "credential-1"
			reader, err := server.Subscribe(ctx, "/pub", authTrackName, nil)
			require.NoError(t, err)

			for i, token := range []string{"credential-1", "credential-2", "credential-3"} {
				sent := time.Now()
				if i > 0 {
					next <- token
				}
				got, err := readGroup(ctx, t, reader)
				require.NoError(t, err, "group %d", i+1)
				assert.Equal(t, token, got)
				t.Logf("%s: group %d delivered in %v", tc.name, i+1, time.Since(sent))
				if i == 0 {
					// Idle long enough that an idle-subscription teardown would show.
					time.Sleep(2 * time.Second)
				}
			}

			// The relay learns that the publisher retracted the broadcast from its
			// own copy of the announcement, and stops reading the auth track then.
			endAnn()
			select {
			case <-serverAnn.Done():
			case <-time.After(2 * time.Second):
				t.Fatal("the server-side announcement did not end after the publisher retracted it")
			}
			// Record, without depending on it, whether the subscription itself
			// ends too.
			endCtx, endCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer endCancel()
			_, err = readGroup(endCtx, t, reader)
			t.Logf("%s: auth read after retraction: %v", tc.name, err)
		})
	}
}

// Q2: a client that only subscribes can serve a session auth track by
// announcing a reserved path; the server sees that announcement and can
// subscribe to it like any other.
func TestContract_ReservedSessionAuthPath(t *testing.T) {
	const reserved = moqt.BroadcastPath("/.qumo/session")
	for _, tc := range contractTransports {
		t.Run(tc.name, func(t *testing.T) {
			addr, roots, sessions := startContractServer(t, moqt.NewTrackMux(0))
			clientMux := moqt.NewTrackMux(0)
			next := make(chan string, 1)
			broadcast := moqt.NewBroadcast()
			require.NoError(t, broadcast.Register(authTrackName, credentialTrack(next)))
			ann, endAnn := moqt.NewAnnouncement(context.Background(), reserved)
			t.Cleanup(endAnn)
			clientMux.Announce(ann, broadcast)
			_ = dialContract(t, tc, addr, roots, clientMux)
			server := acceptServerSession(t, sessions)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			announced, err := server.AcceptAnnounce("/")
			require.NoError(t, err)
			got, err := announced.ReceiveAnnouncement(ctx)
			require.NoError(t, err)
			assert.Equal(t, reserved, got.BroadcastPath())

			next <- "subscriber-credential"
			reader, err := server.Subscribe(ctx, reserved, authTrackName, nil)
			require.NoError(t, err)
			token, err := readGroup(ctx, t, reader)
			require.NoError(t, err)
			assert.Equal(t, "subscriber-credential", token)
		})
	}
}

// Q3: can the server tell which session a SUBSCRIBE came from? The relay
// serves every session from one TrackMux, so authorizing a subscriber needs
// per-connection state reachable from the TrackWriter. On gomoqt v0.20 it is
// not: a Server.ConnContext value reaches neither TrackWriter.Context() nor
// Session.Context(), on either transport.
func TestContract_SubscriberIdentityInServePath(t *testing.T) {
	t.Skip("gomoqt v0.20 does not expose the subscribing session to a TrackHandler; qumo-dev/gomoqt#431")
	for _, tc := range contractTransports {
		t.Run(tc.name, func(t *testing.T) {
			serverMux := moqt.NewTrackMux(0)
			seen := make(chan any, 1)
			serverMux.PublishFunc(context.Background(), "/content", func(tw *moqt.TrackWriter) {
				seen <- tw.Context().Value(connTag{})
				// Open a group so the SUBSCRIBE is answered.
				if g, err := tw.OpenGroup(tw.Context()); err == nil {
					_ = g.Close()
				}
				<-tw.Context().Done()
			})
			addr, roots, _ := startContractServer(t, serverMux)
			client := dialContract(t, tc, addr, roots, moqt.NewTrackMux(0))

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := client.Subscribe(ctx, "/content", "video", nil)
			require.NoError(t, err)

			select {
			case tag := <-seen:
				t.Logf("%s: ConnContext value in TrackWriter.Context(): %v", tc.name, tag)
				assert.Equal(t, client.LocalAddr().String(), tag,
					"per-connection state must reach the serve path to authorize a SUBSCRIBE")
			case <-ctx.Done():
				t.Fatal("SUBSCRIBE never reached the server handler")
			}
		})
	}
}
