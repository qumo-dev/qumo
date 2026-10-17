package ingest

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/internal/cors"
	"github.com/qumo-dev/qumo/internal/envconfig"
	"github.com/qumo-dev/qumo/internal/quicconfig"
	"github.com/qumo-dev/qumo/internal/upstream"
	"github.com/qumo-dev/qumo/token"
)

const (
	defaultRTMPIngestAddr = ":1935"
	defaultRTSPIngestAddr = ":8554"
	defaultRTMPServeAddr  = ":4433"
)

// RunRTMP starts an RTMP ingest server that bridges published streams to
// MoQT, through a relay (RELAY_URL) or on its own MoQT listener. It does not
// join a relay mesh as a peer.
//
// Configuration is read from environment variables:
//
//	RTMP_INGEST_ADDR     - RTMP listen address (default: ":1935")
//	RTMP_SERVE_ADDR      - MoQT listen address for subscribers to dial directly (default: ":4433" without RELAY_URL, none with it)
//	QUMO_AUTH_KEYS       - the key set a publisher's credential (?jwt= on the stream key) is verified against, as the relay reads it (default: none, every publish is accepted)
//	QUMO_AUTH_KEYS_CACHE - a file a downloaded key set is kept in between runs
//	QUMO_RELAY_TOKEN     - bearer token sent to a key-set URL
//	RELAY_URL, RELAY_SIGNING_KEY, RELAY_PUBLISH, RELAY_CA_FILE, RELAY_TLS_INSECURE
//	                     - the relay to publish through (upstream.FromEnv lists them)
//	CERT_FILE            - TLS certificate file for the MoQT listener (default: "certs/server.crt")
//	KEY_FILE             - TLS key file for the MoQT listener (default: "certs/server.key")
//	CORS_ALLOWED_ORIGINS - comma-separated WebTransport origins (default: same-origin only; "*" allows any)
//	QUIC_*               - QUIC transport settings (quicconfig.FromEnv lists them)
func RunRTMP(_ []string) error {
	return runPush("rtmp", "RTMP_INGEST_ADDR", defaultRTMPIngestAddr, "RTMP_SERVE_ADDR",
		func(addr string, mux *moqt.TrackMux, authorize PublishAuthorizer) pushServer {
			return NewRTMPServer(RTMPConfig{Addr: addr, TrackMux: mux, Authorize: authorize})
		})
}

// RunRTSP starts an RTSP ingest server that bridges pushed (ANNOUNCE/RECORD)
// streams to MoQT, through a relay (RELAY_URL) or on its own MoQT listener.
//
// Configuration is read from environment variables, as [RunRTMP] reads them,
// with RTSP_INGEST_ADDR (default: ":8554") and RTSP_SERVE_ADDR in place of
// RTMP_INGEST_ADDR and RTMP_SERVE_ADDR; the publisher's credential is ?jwt=
// on the URL it pushes to.
func RunRTSP(_ []string) error {
	return runPush("rtsp", "RTSP_INGEST_ADDR", defaultRTSPIngestAddr, "RTSP_SERVE_ADDR",
		func(addr string, mux *moqt.TrackMux, authorize PublishAuthorizer) pushServer {
			return NewRTSPServer(RTSPConfig{Addr: addr, TrackMux: mux, Authorize: authorize})
		})
}

// pushServer is an ingest server publishers push to.
type pushServer interface {
	ListenAndServe(ctx context.Context) error
	Shutdown(ctx context.Context) error
}

var (
	_ pushServer = (*RTMPServer)(nil)
	_ pushServer = (*RTSPServer)(nil)
)

// runPush runs the push ingest server newServer makes, named name, until
// SIGINT or SIGTERM, with its broadcasts published as originFromEnv says.
func runPush(name, ingestEnv, defaultIngestAddr, serveEnv string,
	newServer func(addr string, mux *moqt.TrackMux, authorize PublishAuthorizer) pushServer) error {
	ingestAddr := envconfig.String(ingestEnv, defaultIngestAddr)
	quicConfig, err := quicconfig.FromEnv(nil)
	if err != nil {
		return err
	}
	quicconfig.LogEnvSet(name)
	out, err := originFromEnv(name, serveEnv, quicConfig)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	authorize, authName, err := publishAuthorizerFromEnv(ctx, name, out.up.PublishBeneath())
	if err != nil {
		return err
	}
	trackMux := moqt.NewTrackMux(0)
	srv := newServer(ingestAddr, trackMux, authorize)

	log.Println("	Ingest  :", ingestAddr)
	out.logBanner()
	log.Println("	Auth    :", authName)

	go func() {
		if err := srv.ListenAndServe(ctx); err != nil && ctx.Err() == nil {
			slog.Error("ingest server error", "server", name, "err", err)
			cancel()
		}
	}()
	stop := out.serve(ctx, cancel, trackMux)

	<-ctx.Done()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	// not actionable: the process is exiting either way.
	_ = srv.Shutdown(shutdownCtx)
	stop(shutdownCtx)
	return nil
}

// origin is where an ingest command's broadcasts go: a relay it publishes
// through, its own MoQT listener for subscribers to dial directly, or both.
type origin struct {
	name     string
	relayURL string
	// up is nil without RELAY_URL.
	up *upstream.Upstream
	// serveAddr is "" for no listener.
	serveAddr         string
	certFile, keyFile string
	allowedOrigins    []string
	quic              *quic.Config
}

// originFromEnv reads where the ingest command name publishes: through the
// relay upstream.FromEnv reads, and on a MoQT listener at serveEnv, which
// defaults to ":4433" without a relay and to none with one.
func originFromEnv(name, serveEnv string, quicConfig *quic.Config) (*origin, error) {
	up, err := upstream.FromEnv(name, quicConfig)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	serveAddr := defaultRTMPServeAddr
	if up != nil {
		serveAddr = ""
	}
	return &origin{
		name:           name,
		relayURL:       envconfig.String("RELAY_URL", ""),
		up:             up,
		serveAddr:      envconfig.String(serveEnv, serveAddr),
		certFile:       envconfig.String("CERT_FILE", "certs/server.crt"),
		keyFile:        envconfig.String("KEY_FILE", "certs/server.key"),
		allowedOrigins: cors.LoadAllowed(),
		quic:           quicConfig,
	}, nil
}

// logBanner prints where the broadcasts go, for the startup banner.
func (o *origin) logBanner() {
	log.Println("	Relay   :", upstream.RelayName(o.relayURL))
	serve := o.serveAddr
	if serve == "" {
		serve = "none"
	}
	log.Println("	Serve   :", serve)
}

// serve publishes trackMux's broadcasts until ctx ends: through the relay, and
// on the MoQT listener, which cancels the command when it fails. stop shuts
// both down.
func (o *origin) serve(ctx context.Context, cancel context.CancelFunc, trackMux *moqt.TrackMux) (stop func(context.Context)) {
	upstreamDone := make(chan struct{})
	if o.up != nil {
		go func() {
			defer close(upstreamDone)
			o.up.Run(ctx, trackMux)
		}()
	} else {
		close(upstreamDone)
	}
	var moqtSrv *moqt.Server
	if o.serveAddr != "" {
		moqtSrv = newMOQTServer(o.serveAddr, trackMux, o.allowedOrigins, o.quic)
		go func() {
			if err := moqtSrv.ListenAndServeTLS(o.certFile, o.keyFile); err != nil && ctx.Err() == nil {
				slog.Error("MoQT server error", "server", o.name, "err", err)
				cancel()
			}
		}()
	}
	return func(shutdownCtx context.Context) {
		if moqtSrv != nil {
			// not actionable: the process is exiting either way.
			_ = moqtSrv.Shutdown(shutdownCtx)
		}
		<-upstreamDone
	}
}

// newMOQTServer returns an ingest command's own MoQT listener, serving
// trackMux to subscribers that dial it directly. It checks no subscriber:
// with auth on, publish through a relay instead, which does.
func newMOQTServer(addr string, trackMux *moqt.TrackMux, allowedOrigins []string, quicConfig *quic.Config) *moqt.Server {
	wtHandler := &moqt.WebTransportHandler{
		TrackMux:    trackMux,
		CheckOrigin: cors.NewChecker(allowedOrigins),
		Handler: moqt.HandleFunc(func(sess *moqt.Session) {
			defer sess.CloseWithError(moqt.NoError, moqt.NoError.String())
			<-sess.Context().Done()
		}),
	}
	mux := http.NewServeMux()
	mux.Handle("/", wtHandler)
	return &moqt.Server{
		Addr:               addr,
		QUICConfig:         quicConfig,
		WebTransportServer: moqt.NewWebTransportServer(mux),
		TrackMux:           trackMux,
	}
}

// publishAuthorizerFromEnv returns the check every push to the ingest command
// name passes, from the key set QUMO_AUTH_KEYS names, with a label for the
// startup banner. Without a key set every push is accepted, as long as it
// lies beneath beneath (see publishAuthorizer). The key set is refreshed
// until ctx ends.
func publishAuthorizerFromEnv(ctx context.Context, name, beneath string) (PublishAuthorizer, string, error) {
	keys := os.Getenv("QUMO_AUTH_KEYS")
	if keys == "" {
		slog.Warn("auth is off: no key set (QUMO_AUTH_KEYS), so every publish is accepted unchecked", "server", name)
		return publishAuthorizer(nil, beneath), "off: every publish is accepted", nil
	}
	v, err := auth.NewVerifier(auth.VerifierConfig{
		Keys:      keys,
		KeysCache: os.Getenv("QUMO_AUTH_KEYS_CACHE"),
		Token:     os.Getenv("QUMO_RELAY_TOKEN"),
	})
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", name, err)
	}
	go v.Run(ctx)
	return publishAuthorizer(v, beneath), v.Source(), nil
}

// errOutsidePublish refuses a push the relay would not take from the ingest
// server: its path lies outside RELAY_PUBLISH.
var errOutsidePublish = errors.New("the path lies outside RELAY_PUBLISH, beneath which the relay takes this server's broadcasts")

// errNoCredential refuses a push that presented no credential while auth is
// on.
var errNoCredential = errors.New("no credential: present a token that may publish the broadcast as ?jwt=")

// publishAuthorizer returns the check a push passes: its path must lie beneath
// beneath, a directory the relay takes the server's broadcasts beneath (""
// for anywhere), and, with v, its credential must let it publish there; the
// push is then cut when the credential expires. It returns nil, accepting
// every push, for neither.
func publishAuthorizer(v *auth.Verifier, beneath string) PublishAuthorizer {
	if v == nil && beneath == "" {
		return nil
	}
	return func(credential string, path moqt.BroadcastPath) (time.Time, error) {
		if beneath != "" && !strings.HasPrefix(string(path), beneath) {
			return time.Time{}, fmt.Errorf("%s: %w (%s)", path, errOutsidePublish, beneath)
		}
		if v == nil {
			return time.Time{}, nil
		}
		if credential == "" {
			return time.Time{}, errNoCredential
		}
		g, err := v.Check(credential, token.ActionPublish, path)
		if err != nil {
			return time.Time{}, err
		}
		if !g.Allows(token.ActionPublish, path) {
			return time.Time{}, fmt.Errorf("the credential may not publish %s", path)
		}
		return g.Expires(), nil
	}
}
