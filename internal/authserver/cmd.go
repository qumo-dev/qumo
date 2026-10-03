package authserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	defaultAddr     = "127.0.0.1:4440"
	shutdownTimeout = 10 * time.Second
	headerTimeout   = 5 * time.Second
)

type config struct {
	addr      string
	keysFile  string
	anonymous []string
}

// loadConfig reads the environment:
//
//	QUMO_AUTH_ADDR      - listen address (default 127.0.0.1:4440, loopback:
//	                      the relay beside it is the only client)
//	QUMO_AUTH_KEYS_FILE - JWK Set of the trusted Ed25519 public keys, each
//	                      with an optional "prefix" member
//	QUMO_AUTH_ANONYMOUS - comma-separated subtree patterns ("anon/**") that a
//	                      session with no credential may publish and subscribe
//	                      to; "**" opens everything, for development only
//
// At least one of QUMO_AUTH_KEYS_FILE and QUMO_AUTH_ANONYMOUS must be set.
func loadConfig() (config, error) {
	cfg := config{
		addr:     os.Getenv("QUMO_AUTH_ADDR"),
		keysFile: os.Getenv("QUMO_AUTH_KEYS_FILE"),
	}
	if cfg.addr == "" {
		cfg.addr = defaultAddr
	}
	if raw := os.Getenv("QUMO_AUTH_ANONYMOUS"); raw != "" {
		for p := range strings.SplitSeq(raw, ",") {
			p = strings.TrimSpace(p)
			if !validPattern(p) {
				return config{}, fmt.Errorf("QUMO_AUTH_ANONYMOUS: pattern %q: want \"**\" or a path followed by \"/**\"", p)
			}
			cfg.anonymous = append(cfg.anonymous, p)
		}
	}
	if cfg.keysFile == "" && len(cfg.anonymous) == 0 {
		return config{}, errors.New("neither QUMO_AUTH_KEYS_FILE nor QUMO_AUTH_ANONYMOUS is set: " +
			"set QUMO_AUTH_KEYS_FILE to the trusted signing keys, QUMO_AUTH_ANONYMOUS to what sessions " +
			"without a credential may use, or both")
	}
	return cfg, nil
}

// Run serves the auth server until SIGINT or SIGTERM (qumo auth).
func Run(_ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	var keys map[string]Key
	if cfg.keysFile != "" {
		if keys, err = LoadKeys(cfg.keysFile); err != nil {
			return err
		}
	}

	mux := http.NewServeMux()
	// Static keys never change, so a relay has no reason to revalidate.
	mux.Handle("/", &Handler{Keys: keys, Anonymous: cfg.anonymous})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Addr: cfg.addr, Handler: mux, ReadHeaderTimeout: headerTimeout}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	slog.Info("qumo auth listening", "addr", cfg.addr, "keys", len(keys), "anonymous", cfg.anonymous)

	select {
	case err := <-serveErr:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
