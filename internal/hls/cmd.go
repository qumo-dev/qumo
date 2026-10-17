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

	"github.com/qumo-dev/qumo/internal/cors"
	"github.com/qumo-dev/qumo/internal/envconfig"
	"github.com/qumo-dev/qumo/internal/quicconfig"
	"github.com/qumo-dev/qumo/token"

	"github.com/okdaichi/qumo-ledger/ledger"
	"github.com/okdaichi/qumo-ledger/ledger/store"
)

// Run starts the HLS/DASH egress server. Publishers opt in by announcing their
// broadcast beneath [announcePrefix] (ANNOUNCE /.hls/live/cam1 for /live/cam1);
// for each announcement the egress reads the broadcast's catalog, packages each
// received group of LOC frames of every video track into a CMAF (fMP4)
// fragment, writes those fragments into a qumo-ledger track per media track
// ({broadcast}/{track}), and serves the ledger's HLS/DASH renderings over HTTP:
//
//	/{broadcast}/master.m3u8           (HLS master playlist)
//	/{broadcast}/playlist.m3u8         (alias of the master playlist)
//	/{broadcast}/{track}/playlist.m3u8 (HLS media playlist)
//	/{broadcast}/{track}/manifest.mpd  (DASH MPD)
//
// Configuration is read from environment variables (qumo convention):
//
//	HLS_ADDR           - HTTP listen address (default ":8080")
//	LEDGER_URI         - qumo-ledger store URI (default "file:ledger";
//	                     s3://, file://, postgres://, mem:)
//	RELAY_URL          - MoQ relay URL, e.g. "https://host:4433" (default "https://localhost:4433").
//	                     Use "moqt://host:4433" with RELAY_CERT_FILE.
//	HLS_WINDOW        - segments kept in the manifest, i.e. the live window and
//	                     how far back a viewer can seek (default 12). Zero lists
//	                     the whole track, which makes it a recording rather than
//	                     a live stream: players open an unwindowed playlist at
//	                     its first segment and play the history forward.
//	HLS_LIVE_TIMEOUT_S - seconds of silence after which the publisher is treated
//	                     as gone: the feed resubscribes, and manifests answer 503
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
//	                     RELAY_URL, granting subscribe on the whole namespace
//	                     (announcements and the broadcasts they name), so the
//	                     key needs prefix "/"; it presents a fresh one on the
//	                     session before its grant lapses. The way to connect
//	                     to a relay with auth on: a credential fixed in
//	                     RELAY_URL grants for an hour at most.
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
//	QUMO_AUTH_KEYS, QUMO_AUTH_KEYS_CACHE, QUMO_RELAY_TOKEN, HLS_TOKEN_PARAM
//	                     - how viewers are let in (viewerAccessFromEnv lists them):
//	                     with QUMO_AUTH_KEYS, every request needs a token, as
//	                     ?jwt=, that may subscribe to its broadcast, carried by
//	                     the manifests onto every URL they name.
//	CORS_ALLOWED_ORIGINS - comma-separated origins allowed to fetch manifests
//	                     and segments, or "*" for any. Unset disables CORS.
//	                     Required when the player is served from another origin,
//	                     e.g. "http://localhost:5173" for the playground.
func Run(_ []string) error {
	addr := envconfig.String("HLS_ADDR", ":8080")
	ledgerURI := envconfig.String("LEDGER_URI", "file:ledger")

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
		relayURL: envconfig.String("RELAY_URL", "https://localhost:4433"),
		caFile:   envconfig.String("RELAY_CA_FILE", ""),
		insecure: envconfig.String("RELAY_TLS_INSECURE", "false") == "true",
		certFile: envconfig.String("RELAY_CERT_FILE", ""),
		keyFile:  envconfig.String("RELAY_KEY_FILE", ""),

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

	objects, err := store.Open(ctx, ledgerURI)
	if err != nil {
		return fmt.Errorf("hls: open LEDGER_URI: %w", err)
	}

	access, accessName, err := viewerAccessFromEnv(ctx)
	if err != nil {
		return err
	}
	mux := newStreamMux(liveTimeout, access)

	// fed closes once every announcement's feed has finished.
	fed := make(chan struct{})
	go func() {
		defer close(fed)
		runAnnounceEgress(ctx, objects, mux, window, cfg)
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

	slog.Info("hls: serving", "addr", addr, "store", fmt.Sprint(objects), "announce_prefix", announcePrefix,
		"viewers", accessName, "token_param", access.param)

	<-ctx.Done()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	// not actionable: a shutdown error only reports a closed listener on exit.
	_ = httpServer.Shutdown(shutdownCtx)
	// Wait for the feeds, so their epochs close before the process exits.
	<-fed
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
