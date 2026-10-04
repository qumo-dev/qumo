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

	"github.com/okdaichi/qumo-ledger/ledger"
	"github.com/okdaichi/qumo-ledger/ledger/store/fsstore"
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
//	LEDGER_ROOT        - qumo-ledger filesystem store directory (default "./ledger")
//	LEDGER_TRACK       - ledger track path (default "live/cam1/video")
//	RELAY_URL          - MoQ relay URL, e.g. "https://host:4433" (default "https://localhost:4433").
//	                     Use "moqt://host:4433" with RELAY_CERT_FILE. Never
//	                     logged whole: its query is left out.
//	RELAY_TRACK_PATH   - MoQ broadcast path whose catalog to read (default "/hls/live",
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
//	RELAY_KEY_FILE      - the egress's client certificate and key (PEM), from
//	                     the private CA the relay trusts as CA_FILE. The relay
//	                     then serves the egress as a trusted peer: no auth
//	                     server, no credential, no expiry. Set both or neither;
//	                     they need a moqt:// RELAY_URL, since only native QUIC
//	                     sessions can be trusted peers. Re-read on every
//	                     reconnect, so a renewed certificate is picked up.
//	CORS_ALLOWED_ORIGINS - comma-separated origins allowed to fetch manifests
//	                     and segments, or "*" for any. Unset disables CORS.
//	                     Required when the player is served from another origin,
//	                     e.g. "http://localhost:5173" for the playground.
func Run(_ []string) error {
	addr := envconfig.String("HLS_ADDR", ":8080")
	root := envconfig.String("LEDGER_ROOT", "./ledger")
	ledgerTrack := ledger.TrackPath(envconfig.String("LEDGER_TRACK", "live/cam1/video"))

	window, err := envUint("HLS_WINDOW", 12)
	if err != nil {
		return fmt.Errorf("invalid HLS_WINDOW: %w", err)
	}
	liveTimeoutSec, err := envUint("HLS_LIVE_TIMEOUT_S", 10)
	if err != nil {
		return fmt.Errorf("invalid HLS_LIVE_TIMEOUT_S: %w", err)
	}
	liveTimeout := time.Duration(liveTimeoutSec) * time.Second

	cfg := feedConfig{
		relayURL:  envconfig.String("RELAY_URL", "https://localhost:4433"),
		trackPath: envconfig.String("RELAY_TRACK_PATH", "/hls/live"),
		trackName: envconfig.String("RELAY_TRACK_NAME", "video"),
		caFile:    envconfig.String("RELAY_CA_FILE", ""),
		insecure:  envconfig.String("RELAY_TLS_INSECURE", "false") == "true",
		certFile:  envconfig.String("RELAY_CERT_FILE", ""),
		keyFile:   envconfig.String("RELAY_KEY_FILE", ""),

		liveTimeout: liveTimeout,
	}
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("hls: %w", err)
	}

	// Shared between the feed, which records each committed group, and the
	// server, which will not claim a stream is live without them.
	var live liveness

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	store, err := fsstore.New(root)
	if err != nil {
		return fmt.Errorf("hls: open ledger store: %w", err)
	}

	// Read the catalog before opening the track or building the handler: the
	// schema and fMP4 init both come from it. The publisher and the egress are
	// started independently (there is no ordering guarantee between them), so a
	// broadcast that does not exist yet — or a catalog that does not carry its
	// init segment yet — is expected at startup, not fatal. connectWithRetry
	// backs off until the publisher is fully ready.
	session, media, err := connectWithRetry(ctx, cfg)
	if err != nil {
		return err
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
	// publisher numbers its own groups from zero. Continuing that epoch would
	// collide with the IDs already committed, and a sealed group is immutable —
	// every group of this stream would be refused while the manifest went on
	// serving the previous run's. Opening an epoch gives this producer its own
	// sequence space.
	if adopted {
		if err := writer.NewEpoch(ctx); err != nil {
			return fmt.Errorf("hls: begin epoch on an existing track: %w", err)
		}
		slog.Info("hls: adopted an existing track, new epoch opened", "track", ledgerTrack)
	}

	go runFeed(ctx, cancel, session, media, writer, &live, cfg)

	// The init segment is derived from the catalog, so it exists as soon as the
	// track is described — before any media arrives. The handler is built once,
	// complete, and never serves a manifest that lacks #EXT-X-MAP.
	// The window is what makes this a live stream rather than a recording. The
	// ledger keeps every group, so an unwindowed manifest lists the whole track
	// from its first segment — and a player opening that starts at the oldest
	// one and plays the history forward, however long ago it was captured.
	// Windowing keeps the manifest at the live edge; the older segments stay
	// fetchable for anyone holding their URLs.
	handler, err := stream.NewHandler(track, stream.Options{
		InitSegment: stream.InitSegment{Bytes: media.packager.Init()},
		Window:      int(window),
		// This egress opens an epoch per producer lifetime — on startup over an
		// existing track, and on every reconnect — so the newest one is this
		// publisher's current session. Anything older is a session that has
		// ended, and listing it would put a finished stream in front of a live
		// viewer.
		EpochWindow: 1,
	})
	if err != nil {
		return fmt.Errorf("hls: build stream handler: %w", err)
	}

	httpServer := &http.Server{
		Addr: addr,
		// CORS outermost so its headers reach every response, including the
		// 503 a stale feed produces — a browser cannot read a refusal it is not
		// allowed to see, and would report it as a network failure instead.
		Handler:           withCORS(withLiveness(handler, &live, liveTimeout), cors.LoadAllowed()),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("hls: http serve", "err", err)
			cancel()
		}
	}()

	slog.Info("hls: serving", "addr", addr, "track", ledgerTrack)

	<-ctx.Done()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	// not actionable: a shutdown error only reports a closed listener on exit.
	_ = httpServer.Shutdown(shutdownCtx)
	return nil
}

// openTrack creates the ledger track if absent, adopting an existing one. The
// schema comes from the MSF catalog (its TimeSource is ingest: the feed stamps
// Wallclock from its own clock). The second result reports that an existing
// track was adopted rather than created, which the caller needs because a track
// with someone else's groups in it cannot simply be written to.
func openTrack(ctx context.Context, store *fsstore.Store, path ledger.TrackPath, schema ledger.TrackSchema) (*ledger.Track, bool, error) {
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
