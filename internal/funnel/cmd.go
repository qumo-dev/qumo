// Package funnel records what arrives over HTTP and serves it as MoQ tracks.
//
// Many contributors POST records into one track. Each record is committed to a
// qumo-ledger track first and then sent to the track's MoQ subscribers, so a
// subscriber only ever sees a record that is stored.
//
//	POST   /announce                    {"broadcast_path": "/room/123", "track_name": "chat"}
//	                                    → 201, Location: contributions/{id}
//	POST   /contributions/{id}/records  the record's payload, one JSON value
//	DELETE /contributions/{id}          end the contribution
//
// With a key set, every request carries a qumo credential as a bearer token,
// and the credential must grant publishing at the broadcast path.
//
// The funnel publishes through a relay it dials as a client, so subscribers
// reach its tracks on the relay, and it can also serve them itself. A
// subscriber reads the track at that broadcast path and track name. Each record
// is one group holding one frame, the payload as it was sent, and the group's
// sequence is one more than the sequence of the ledger group that stores it.
package funnel

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/okdaichi/qumo-ledger/ingest"
	"github.com/okdaichi/qumo-ledger/ledger"
	"github.com/okdaichi/qumo-ledger/ledger/store"
	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/internal/cors"
	"github.com/qumo-dev/qumo/internal/envconfig"
)

const (
	defaultIngestAddr = ":8090"
	defaultServeAddr  = ":4433"
)

// Run starts the funnel: the HTTP server contributors POST to, a session to
// the relay that publishes what it records, and, optionally, its own MoQT
// listener. It does not join a relay mesh as a peer.
//
// Configuration is read from environment variables:
//
//	FUNNEL_ADDR          - HTTP listen address for announce and record (default: ":8090")
//	RELAY_URL            - the relay to publish through, dialed as a client, e.g. "https://relay:4433" or "moqt://relay:4433"; it may carry a credential as ?jwt=
//	RELAY_SIGNING_KEY    - a signing key (qumo auth keygen) the funnel signs a fresh relay credential with on every dial, replacing ?jwt=
//	RELAY_PUBLISH        - the path that credential grants publishing at, e.g. "room"; set with RELAY_SIGNING_KEY
//	RELAY_CA_FILE        - PEM cert to trust as the relay's root (default: the system roots)
//	RELAY_TLS_INSECURE   - skip relay TLS verification, for a self-signed dev relay (default: "false")
//	FUNNEL_SERVE_ADDR    - MoQT listen address for subscribers to dial directly (default: ":4433" without RELAY_URL, none with it)
//	LEDGER_URI           - where records are stored: "file:///var/lib/qumo", "postgres://...", "s3://bucket/prefix?region=...", or empty for memory (default: memory, lost on exit)
//	CERT_FILE            - TLS certificate file for the MoQT listener (default: "certs/server.crt")
//	KEY_FILE             - TLS key file for the MoQT listener (default: "certs/server.key")
//	CORS_ALLOWED_ORIGINS - comma-separated origins allowed to call the HTTP endpoints and to open WebTransport (default: same-origin only; "*" allows any)
//	QUMO_AUTH_KEYS       - the key set contributors' credentials are verified against, as the relay reads it (default: none, every request is accepted)
//	QUMO_AUTH_KEYS_CACHE - a file a downloaded key set is kept in between runs
//	QUMO_RELAY_TOKEN     - bearer token sent to a key-set URL
//
// The HTTP listener is plain HTTP: terminate TLS in front of it.
func Run(_ []string) error {
	ingestAddr := envconfig.String("FUNNEL_ADDR", defaultIngestAddr)
	relayURL := envconfig.String("RELAY_URL", "")
	serveAddr := defaultServeAddr
	if relayURL != "" {
		serveAddr = ""
	}
	serveAddr = envconfig.String("FUNNEL_SERVE_ADDR", serveAddr)
	ledgerURI := envconfig.String("LEDGER_URI", "")
	certFile := envconfig.String("CERT_FILE", "certs/server.crt")
	keyFile := envconfig.String("KEY_FILE", "certs/server.key")
	allowedOrigins := cors.LoadAllowed()

	var up *upstream
	if relayURL != "" {
		var err error
		up, err = newUpstream(RelayConfig{
			URL:            relayURL,
			CAFile:         envconfig.String("RELAY_CA_FILE", ""),
			Insecure:       envconfig.String("RELAY_TLS_INSECURE", "false") == "true",
			SigningKeyFile: envconfig.String("RELAY_SIGNING_KEY", ""),
			Publish:        envconfig.String("RELAY_PUBLISH", ""),
		})
		if err != nil {
			return fmt.Errorf("funnel: %w", err)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	objects, storeName, err := openStore(ctx, ledgerURI)
	if err != nil {
		return err
	}

	authName := "off: every request is accepted"
	var verifier *auth.Verifier
	if keys := os.Getenv("QUMO_AUTH_KEYS"); keys != "" {
		verifier, err = auth.NewVerifier(auth.VerifierConfig{
			Keys:      keys,
			KeysCache: os.Getenv("QUMO_AUTH_KEYS_CACHE"),
			Token:     os.Getenv("QUMO_RELAY_TOKEN"),
		})
		if err != nil {
			return err
		}
		go verifier.Run(ctx)
		authName = verifier.Source()
	} else {
		slog.Warn("funnel: auth is off: no key set (QUMO_AUTH_KEYS), so every contributor is accepted unchecked")
	}

	trackMux := moqt.NewTrackMux(0)
	handler, err := NewHandler(ctx, objects, trackMux, verifier)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Addr:              ingestAddr,
		Handler:           withCORS(handler, allowedOrigins),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Println("	Ingest  :", ingestAddr)
	log.Println("	Relay   :", relayName(relayURL))
	log.Println("	Serve   :", orNone(serveAddr))
	log.Println("	Ledger  :", storeName)
	log.Println("	Auth    :", authName)

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("funnel HTTP server error", "err", err)
			cancel()
		}
	}()
	upstreamDone := make(chan struct{})
	if up != nil {
		go func() {
			defer close(upstreamDone)
			up.run(ctx, trackMux)
		}()
	} else {
		close(upstreamDone)
	}
	var moqtSrv *moqt.Server
	if serveAddr != "" {
		moqtSrv = newMOQTServer(serveAddr, trackMux, allowedOrigins)
		go func() {
			if err := moqtSrv.ListenAndServeTLS(certFile, keyFile); err != nil && ctx.Err() == nil {
				slog.Error("MoQT server error", "err", err)
				cancel()
			}
		}()
	}

	<-ctx.Done()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	// not actionable: the process is exiting either way.
	_ = httpSrv.Shutdown(shutdownCtx)
	if moqtSrv != nil {
		// not actionable: the process is exiting either way.
		_ = moqtSrv.Shutdown(shutdownCtx)
	}
	<-upstreamDone
	return nil
}

// newMOQTServer returns the funnel's own MoQT listener, serving trackMux to
// subscribers that dial it directly.
func newMOQTServer(addr string, trackMux *moqt.TrackMux, allowedOrigins []string) *moqt.Server {
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
		WebTransportServer: moqt.NewWebTransportServer(mux),
		TrackMux:           trackMux,
	}
}

// relayName describes RELAY_URL for the startup log without its credential.
func relayName(relayURL string) string {
	if relayURL == "" {
		return "none"
	}
	u, err := url.Parse(relayURL)
	if err != nil {
		return "invalid"
	}
	return u.Scheme + "://" + u.Host + u.Path
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// NewHandler builds the announce and record handler over objects, wired to
// publish what it commits on trackMux. Tracks objects already holds are
// published with their latest record first. Broadcasts stay announced until
// ctx ends. A nil verifier accepts every contributor.
func NewHandler(ctx context.Context, objects store.Store, trackMux *moqt.TrackMux, verifier *auth.Verifier) (http.Handler, error) {
	out := newEgress(ctx, trackMux)
	restored, err := restore(ctx, objects, out)
	if err != nil {
		return nil, err
	}
	if restored > 0 {
		slog.Info("funnel: restored recorded tracks", "tracks", restored)
	}
	return ingest.NewHandler(objects, ingest.Options{
		Authorize: authorizer(verifier),
		Challenge: "Bearer",
		OnAnnounce: func(_ context.Context, t ingest.Track) {
			out.announce(moqt.BroadcastPath(t.BroadcastPath)).track(moqt.TrackName(t.TrackName))
		},
		OnRecord: func(_ context.Context, t ingest.Track, g ledger.GroupInfo, payload []byte) {
			out.publish(moqt.BroadcastPath(t.BroadcastPath), moqt.TrackName(t.TrackName), group{
				seq:     moqt.GroupSequence(g.ID.Sequence() + 1),
				payload: payload,
			})
		},
		Logger: slog.Default(),
	})
}

// withCORS lets a browser on an allowed origin call h with a bearer
// credential and read the Location of a contribution, and answers its
// preflight request.
func withCORS(h http.Handler, allowed []string) http.Handler {
	allow := cors.NewChecker(allowed)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		if origin := r.Header.Get("Origin"); origin != "" && allow(r) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
			w.Header().Set("Access-Control-Expose-Headers", "Location")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}
