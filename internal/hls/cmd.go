package hls

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/qumo-dev/qumo/internal/cors"
	"github.com/qumo-dev/qumo/internal/envconfig"
	"github.com/qumo-dev/qumo/internal/quicconfig"
	"github.com/qumo-dev/qumo/token"

	"github.com/okdaichi/qumo-ledger/ledger"
	"github.com/okdaichi/qumo-ledger/ledger/store"
	"github.com/okdaichi/qumo-ledger/stream"
)

// Run starts the HLS/DASH egress server: it subscribes to a MoQ track's catalog
// to learn its schema and fMP4 init, packages each received group of LOC frames
// into a CMAF (fMP4) fragment, writes those fragments into a qumo-ledger track,
// and serves the ledger's HLS/DASH renderings over HTTP.
//
// Configuration is read from environment variables (qumo convention):
//
//	HLS_ADDR           - HTTP listen address (default ":8080")
//	LEDGER_URI         - qumo-ledger store URI (default "file:ledger";
//	                     s3://, file://, postgres://, mem:)
//	LEDGER_TRACK       - ledger track path for static mode (default "live/cam1/video")
//	HLS_ANNOUNCE_PREFIX - announce prefix to accept (default "/.hls/").
//	                     Publishers opt-in by announcing broadcast paths beneath
//	                     this prefix (e.g. ANNOUNCE /.hls/live/cam1). The egress
//	                     strips this prefix to discover the canonical broadcast path
//	                     (/live/cam1), discovers all media tracks from its catalog,
//	                     and routes:
//	                       /{broadcast}/master.m3u8          (HLS master playlist)
//	                       /{broadcast}/playlist.m3u8        (convenience alias)
//	                       /{broadcast}/{track}/playlist.m3u8 (HLS media playlist)
//	                     Unset or empty ("") disables announce listening and
//	                     uses RELAY_TRACK_PATH directly.
//	RELAY_URL          - MoQ relay URL, e.g. "https://host:4433" (default "https://localhost:4433").
//	                     Use "moqt://host:4433" with RELAY_CERT_FILE.
//	RELAY_TRACK_PATH   - MoQ broadcast path whose catalog to read when announce
//	                     listening is disabled (default "/hls/live",
//	                     the playground's HLS scenario)
//	RELAY_TRACK_NAME   - media track name in the catalog to relay (default "video")
//	HLS_WINDOW         - segments kept in the manifest, i.e. the live window and
//	                     how far back a viewer can seek (default 12). Zero lists
//	                     the whole track, which makes it a recording rather than
//	                     a live stream: players open an unwindowed playlist at
//	                     its first segment and play the history forward.
//	HLS_LIVE_TIMEOUT_S - seconds of silence after which the publisher is treated
//	                     as gone: the feed reconnects, and manifests answer 503
//	                     rather than describe media that stopped arriving
//	                     (default 10)
//	RELAY_CA_FILE       - PEM cert to trust as the relay's root, overriding the
//	                     system roots (e.g. the relay's own cert when it is
//	                     self-signed). Unset means verify against the system root
//	                     store.
//	RELAY_TLS_INSECURE  - skip relay TLS verification entirely, for a self-signed
//	                     dev relay such as seed-moq (default "false"; the egress
//	                     verifies the relay's certificate by default). Dominates
//	                     RELAY_CA_FILE when both are set.
//	RELAY_CERT_FILE,
//	RELAY_SIGNING_KEY   - a signing key (qumo auth keygen) the relay's key
//	                     set trusts. The egress signs a fresh credential with
//	                     it on every connect, in place of any ?jwt= in
//	                     RELAY_URL, granting subscribe on RELAY_TRACK_PATH
//	                     alone, and presents a fresh one on the session
//	                     before its grant lapses. The way to connect to a
//	                     relay with auth on: a credential fixed in RELAY_URL
//	                     grants for an hour at most.
//	RELAY_CERT_FILE,
//	RELAY_KEY_FILE      - instead of a credential, a peer certificate and key
//	                     (PEM) for the egress, from the relay CA the relay
//	                     trusts as CA_FILE, with the peering name, and not
//	                     the relay's own. The relay then serves the egress as
//	                     a relay peer: no credential, no expiry, and nothing
//	                     it does is checked. Not with RELAY_SIGNING_KEY or
//	                     ?jwt=. Set both or neither; they need a moqt://
//	                     RELAY_URL, since only native QUIC sessions present a
//	                     certificate. Re-read on every reconnect, so a renewed
//	                     certificate is picked up.
//	QUIC_*               - QUIC transport settings of the session to the relay
//	                     (quicconfig.FromEnv lists them).
//	CORS_ALLOWED_ORIGINS - comma-separated origins allowed to fetch manifests
//	                     and segments, or "*" for any. Unset disables CORS.
//	                     Required when the player is served from another origin,
//	                     e.g. "http://localhost:5173" for the playground.
func Run(_ []string) error {
	addr := envconfig.String("HLS_ADDR", ":8080")
	ledgerURI := envconfig.String("LEDGER_URI", defaultLedgerURI)
	ledgerTrack := ledger.TrackPath(envconfig.String("LEDGER_TRACK", "live/cam1/video"))
	announcePrefix := envconfig.String("HLS_ANNOUNCE_PREFIX", "/.hls/")

	window, err := envUint("HLS_WINDOW", 12)
	if err != nil {
		return fmt.Errorf("invalid HLS_WINDOW: %w", err)
	}
	liveTimeoutSec, err := envUint("HLS_LIVE_TIMEOUT_S", 10)
	if err != nil {
		return fmt.Errorf("invalid HLS_LIVE_TIMEOUT_S: %w", err)
	}
	liveTimeout := time.Duration(liveTimeoutSec) * time.Second
	quicConfig, err := quicconfig.FromEnv(nil)
	if err != nil {
		return fmt.Errorf("hls: %w", err)
	}
	quicconfig.LogEnvSet("hls")

	cfg := feedConfig{
		relayURL:       envconfig.String("RELAY_URL", "https://localhost:4433"),
		announcePrefix: announcePrefix,
		trackPath:      envconfig.String("RELAY_TRACK_PATH", "/hls/live"),
		trackName:      envconfig.String("RELAY_TRACK_NAME", "video"),
		caFile:         envconfig.String("RELAY_CA_FILE", ""),
		insecure:       envconfig.String("RELAY_TLS_INSECURE", "false") == "true",
		certFile:       envconfig.String("RELAY_CERT_FILE", ""),
		keyFile:        envconfig.String("RELAY_KEY_FILE", ""),

		liveTimeout: liveTimeout,
		quic:        quicConfig,
	}
	if keyFile := envconfig.String("RELAY_SIGNING_KEY", ""); keyFile != "" {
		key, err := token.LoadSigningKey(keyFile)
		if err != nil {
			return fmt.Errorf("hls: RELAY_SIGNING_KEY: %w", err)
		}
		cfg.signingKey = &key
	}
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("hls: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	store, storeName, err := openStore(ctx, ledgerURI)
	if err != nil {
		return fmt.Errorf("hls: %w", err)
	}

	mux := newStreamMux(liveTimeout, nil)

	// If announce mode is enabled (default "/.hls/"), start announce egress.
	// Otherwise, fall back to the legacy single-track feed.
	// fed reports the feed's fatal error, nil when it stopped with ctx.
	fed := make(chan error, 1)
	go func() {
		var err error
		if cfg.announcePrefix != "" {
			runAnnounceEgress(ctx, store, mux, window, cfg)
		} else {
			err = runStaticFeed(ctx, cancel, store, mux, ledgerTrack, window, cfg)
		}
		if err != nil {
			// Stop serving: cancelling the root context shuts the HTTP
			// server down too, and Run reports err.
			cancel()
		}
		fed <- err
	}()

	httpServer := &http.Server{
		Addr: addr,
		// CORS outermost so its headers reach every response, including the
		// 503 a stale feed produces.
		Handler:           withCORS(mux, cors.LoadAllowed()),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("hls: http serve", "err", err)
			cancel()
		}
	}()

	slog.Info("hls: serving", "addr", addr, "store", storeName, "announce_prefix", cfg.announcePrefix)

	<-ctx.Done()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	// not actionable: a shutdown error only reports a closed listener on exit.
	_ = httpServer.Shutdown(shutdownCtx)
	// Wait for the feed, so its epochs close before the process exits, and
	// exit non-zero when it is what stopped the egress.
	return <-fed
}

// runStaticFeed feeds a single static track configured via RELAY_TRACK_PATH and
// mounts it on mux's fallback, for backward compatibility when announce mode is
// disabled. It returns an error when the egress cannot go on, nil once ctx is
// done.
func runStaticFeed(ctx context.Context, cancel context.CancelFunc, store store.Store, mux *streamMux, ledgerTrack ledger.TrackPath, window uint64, cfg feedConfig) error {
	var live liveness

	// The publisher and the egress start independently, so a broadcast that
	// does not exist yet — or a catalog without its init segment yet — is
	// expected at startup, not fatal: connectWithRetry backs off until the
	// publisher is ready.
	session, media, err := connectWithRetry(ctx, cfg)
	if err != nil {
		// ctx was cancelled while waiting for the publisher.
		return nil
	}
	defer func() {
		// not actionable: the egress is stopping regardless of the close outcome.
		_ = session.CloseWithError(moqt.NoError, "hls egress stopped")
	}()

	track, adopted, err := openTrack(ctx, store, ledgerTrack, media.schema)
	if err != nil {
		return err
	}
	writer, err := track.Writer(ctx)
	if err != nil {
		return fmt.Errorf("hls: open writer: %w", err)
	}

	// A track that already held groups was filled by an earlier run, and this
	// publisher numbers its own groups from zero; continuing that epoch would
	// collide with the sealed groups already committed. Opening an epoch gives
	// this producer its own sequence space.
	if adopted {
		if err := writer.NewEpoch(ctx); err != nil {
			return fmt.Errorf("hls: begin epoch on an existing track: %w", err)
		}
		slog.Info("hls: adopted an existing track, new epoch opened", "track", ledgerTrack)
	}

	// The window keeps the manifest at the live edge rather than listing the
	// whole ledger track. EpochWindow 1: this egress opens an epoch per
	// producer lifetime, so anything older is a session that has ended.
	handler, err := stream.NewHandler(track, stream.Options{
		InitSegment: stream.InitSegment{Bytes: media.packager.Init()},
		Window:      int(window),
		EpochWindow: 1,
	})
	if err != nil {
		return fmt.Errorf("hls: build stream handler: %w", err)
	}

	mux.setFallback(withLiveness(handler, &live, cfg.liveTimeout))

	runFeed(ctx, cancel, session, media, writer, &live, cfg)
	return nil
}

// openTrack creates the ledger track if absent, adopting an existing one. The
// schema comes from the MSF catalog (its TimeSource is ingest: the feed stamps
// Wallclock from its own clock). The second result reports that an existing
// track was adopted rather than created, which the caller needs because a track
// with someone else's groups in it cannot simply be written to.
func openTrack(ctx context.Context, store store.Store, path ledger.TrackPath, schema ledger.TrackSchema) (*ledger.Track, bool, error) {
	track, err := ledger.Create(ctx, store, path, schema, ledger.Config{})
	if err == nil {
		return track, false, nil
	}
	if !errors.Is(err, ledger.ErrTrackExists) {
		return nil, false, fmt.Errorf("hls: create track: %w", err)
	}

	track, err = ledger.Open(ctx, store, path, ledger.Config{})
	if err != nil {
		return nil, false, fmt.Errorf("hls: open track: %w", err)
	}
	return track, true, nil
}

func envUint(key string, def uint64) (uint64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}
