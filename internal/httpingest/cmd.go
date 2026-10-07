// Package httpingest records what arrives over HTTP and serves it as MoQ
// tracks.
//
// Many contributors POST records into one track. Each record is committed to a
// qumo-ledger track first and then sent to the track's MoQ subscribers, so a
// subscriber only ever sees a record that is stored.
//
//	POST /announce  {"broadcast_path": "/room/123", "track_name": "chat", "name": "alice"}
//	POST /record    {"broadcast_path": "/room/123", "track_name": "chat", "name": "alice", "payload": ...}
//
// A subscriber reads the track at that broadcast path and track name. Each
// record is one group holding one frame, the JSON object
// {"name": ..., "payload": ...}, and the group's sequence is one more than the
// sequence of the ledger group that stores the record.
package httpingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/okdaichi/qumo-ledger/ingest"
	"github.com/okdaichi/qumo-ledger/ledger/store"
	"github.com/okdaichi/qumo-ledger/ledger/store/fsstore"
	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/qumo-dev/qumo/internal/cors"
	"github.com/qumo-dev/qumo/internal/envconfig"
)

const (
	defaultIngestAddr = ":8090"
	defaultServeAddr  = ":4433"
	defaultLedgerRoot = "./ledger"
)

// Run starts the HTTP ingest server and the MoQT origin that serves what it
// records. It does not join a relay mesh.
//
// Configuration is read from environment variables:
//
//	HTTP_INGEST_ADDR     - HTTP listen address for announce and record (default: ":8090")
//	HTTP_SERVE_ADDR      - MoQT listen address (default: ":4433")
//	LEDGER_ROOT          - qumo-ledger filesystem store directory (default: "./ledger")
//	CERT_FILE            - TLS certificate file for MoQT (default: "certs/server.crt")
//	KEY_FILE             - TLS key file for MoQT (default: "certs/server.key")
//	CORS_ALLOWED_ORIGINS - comma-separated origins allowed to POST and to open WebTransport (default: same-origin only; "*" allows any)
//
// The HTTP listener is plain HTTP and checks no credentials: run it behind
// whatever authenticates contributors.
func Run(_ []string) error {
	ingestAddr := envconfig.String("HTTP_INGEST_ADDR", defaultIngestAddr)
	serveAddr := envconfig.String("HTTP_SERVE_ADDR", defaultServeAddr)
	root := envconfig.String("LEDGER_ROOT", defaultLedgerRoot)
	certFile := envconfig.String("CERT_FILE", "certs/server.crt")
	keyFile := envconfig.String("KEY_FILE", "certs/server.key")
	allowedOrigins := cors.LoadAllowed()

	objects, err := fsstore.New(root)
	if err != nil {
		return fmt.Errorf("open ledger store %s: %w", root, err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	trackMux := moqt.NewTrackMux(0)
	handler, err := NewHandler(ctx, objects, trackMux)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Addr:              ingestAddr,
		Handler:           withCORS(handler, allowedOrigins),
		ReadHeaderTimeout: 10 * time.Second,
	}

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
	moqtSrv := &moqt.Server{
		Addr:               serveAddr,
		WebTransportServer: moqt.NewWebTransportServer(mux),
		TrackMux:           trackMux,
	}

	log.Println("	Ingest  :", ingestAddr)
	log.Println("	Serve   :", serveAddr)
	log.Println("	Ledger  :", root)

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP ingest server error", "err", err)
			cancel()
		}
	}()
	go func() {
		if err := moqtSrv.ListenAndServeTLS(certFile, keyFile); err != nil && ctx.Err() == nil {
			slog.Error("MoQT server error", "err", err)
			cancel()
		}
	}()

	<-ctx.Done()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	_ = httpSrv.Shutdown(shutdownCtx)
	_ = moqtSrv.Shutdown(shutdownCtx)

	return nil
}

// NewHandler builds the announce and record handler over objects, wired to
// publish what it commits on trackMux. Broadcasts stay announced until ctx
// ends.
func NewHandler(ctx context.Context, objects store.Store, trackMux *moqt.TrackMux) (http.Handler, error) {
	out := newEgress(ctx, trackMux)
	return ingest.NewHandler(objects, ingest.Options{
		OnAnnounce: func(_ context.Context, a ingest.Announced) {
			out.announce(moqt.BroadcastPath(a.BroadcastPath))
		},
		OnRecord: func(_ context.Context, rec ingest.Recorded) {
			payload, err := json.Marshal(rec.Record)
			if err != nil {
				slog.Error("httpingest: encode record", "track", rec.Track(), "error", err)
				return
			}
			out.publish(moqt.BroadcastPath(rec.BroadcastPath), moqt.TrackName(rec.TrackName), group{
				seq:     moqt.GroupSequence(rec.Group.ID.Sequence() + 1),
				payload: payload,
			})
		},
		Logger: slog.Default(),
	})
}

// withCORS lets a browser on an allowed origin POST to h, and answers its
// preflight request.
func withCORS(h http.Handler, allowed []string) http.Handler {
	allow := cors.NewChecker(allowed)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		if origin := r.Header.Get("Origin"); origin != "" && allow(r) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}
