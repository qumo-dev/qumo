package playground

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/qumo-dev/qumo/internal/cors"
)

// Defaults for the local demo. The UI and relay are loopback-only by default;
// pass --ui-addr / --relay-addr to expose them on a public host. The browser-
// facing relay URL is derived automatically from whatever host the UI is opened
// at (no --host flag).
const (
	defaultUIAddr    = "127.0.0.1:8080"
	defaultRelayAddr = "127.0.0.1:4433"
)

// Options configures a playground run. Defaults are applied for any zero field
// except Assets and StartRelay, which are required.
type Options struct {
	// UIAddr is the HTTP address serving the embedded UI + /config.
	UIAddr string
	// RelayAddr is the WebTransport address the in-process relay binds.
	RelayAddr string
	// CertDir overrides the dev-cert cache location ("" = per-user cache dir).
	CertDir string
	// Assets is the embedded dist filesystem (sub-rooted at its content root).
	Assets fs.FS
	// StartRelay launches the relay in-process. It is expected to block until
	// the relay shuts down. The orchestrator sets the relay's env vars
	// (RELAY_ADDR, CERT_FILE, KEY_FILE, RELAY_NAME) before calling it.
	StartRelay func() error
}

// Run starts a local demo: ensures a dev cert, launches the relay in-process,
// serves the embedded UI + /config, prints one URL, and blocks until SIGINT or
// SIGTERM. It returns a non-nil error only if startup fails; on a signal it
// shuts both servers down and returns nil.
func Run(ctx context.Context, o Options) error {
	if o.Assets == nil {
		return errors.New("playground: Assets is required")
	}
	if o.StartRelay == nil {
		return errors.New("playground: StartRelay is required")
	}
	if o.UIAddr == "" {
		o.UIAddr = defaultUIAddr
	}
	if o.RelayAddr == "" {
		o.RelayAddr = defaultRelayAddr
	}

	// 1. Ensure a dev cert whose hash the browser will pin and whose files the
	//    relay will load. The cert is minted for localhost/loopback; that's fine
	//    for a public host too, because WebTransport's serverCertificateHashes
	//    pins the cert by its SHA-256 hash and skips hostname validation.
	cert, err := EnsureCert(o.CertDir)
	if err != nil {
		return err
	}
	slog.Info("dev certificate ready",
		"cert", cert.CertFile, "hash", cert.HashHex)

	// 2. Configure the relay via its env vars before it reads them.
	if err := configureRelayEnv(o.RelayAddr, cert); err != nil {
		return err
	}

	// 3. Launch the relay in-process. relay.Run blocks and installs its own
	//    signal handler; both that handler and ours receive SIGINT/SIGTERM, so
	//    both servers shut down in parallel.
	relayDone := make(chan error, 1)
	go func() {
		relayDone <- o.StartRelay()
	}()

	// 4. Launch the UI server. The relayUrl served at /config is derived per
	//    request from the browser's Host header, so the server only needs the
	//    relay port (not a preconfigured host).
	ui := NewServerWithCerts(o.UIAddr, portOf(o.RelayAddr), cert.HashHex, cert.CertFile, cert.KeyFile, o.Assets)

	// A bundle-less dist (a checkout whose web UI was never built, #376) would
	// white-screen the browser tab with an opaque MIME error. The relay still
	// works, so continue in degraded mode but say so up front; the server,
	// which computed the same check at construction (assetsErr), serves an
	// explanatory error page in place of the UI.
	if ui.assetsErr != nil {
		slog.Warn("playground web UI is not bundled; the browser will show an error page",
			"err", ui.assetsErr)
	}

	uiDone := make(chan error, 1)
	go func() {
		uiDone <- ui.ListenAndServe()
	}()

	slog.Info("playground ready",
		"url", ui.URL(), "relay_addr", o.RelayAddr,
		"note", "relayUrl is derived per-request from the browser's Host")
	// Print the URL the user should open. When the UI binds a wildcard address
	// (public hosting), fall back to "localhost" so the printed URL is at least
	// locally reachable; the public URL is whatever the user/proxy serves.
	if _, err := os.Stdout.WriteString(uiDisplayURL(o.UIAddr) + "\n"); err != nil {
		slog.Warn("failed to write playground URL to stdout", "err", err)
	}

	// 5. Block until a signal arrives or either server exits on its own.
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	select {
	case <-ctx.Done():
		// Graceful shutdown initiated by the user.
	case err := <-relayDone:
		slog.Error("relay exited, shutting down", "err", err)
	case err := <-uiDone:
		slog.Error("UI server exited, shutting down", "err", err)
	}

	// 6. Tear down. The relay tears itself down via its own signal handler when
	//    ctx (and thus SIGINT) fired; wait for it with a bounded timeout so a
	//    stuck relay can't hang the process past its own ~10s shutdown window.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer shutdownCancel()
	if err := ui.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("UI server shutdown error", "err", err)
	}

	select {
	case <-relayDone:
	case <-time.After(12 * time.Second):
		slog.Warn("relay did not shut down within 12s; exiting anyway")
	}
	return nil
}

// configureRelayEnv sets the env vars the relay reads, on the current process.
// PEERS is explicitly cleared so a user-exported peer list can't pull the
// single-node demo into a cluster dial.
func configureRelayEnv(relayAddr string, cert *Cert) error {
	if err := os.Setenv("RELAY_ADDR", relayAddr); err != nil {
		return err
	}
	if err := os.Setenv("CERT_FILE", cert.CertFile); err != nil {
		return err
	}
	if err := os.Setenv("KEY_FILE", cert.KeyFile); err != nil {
		return err
	}
	// RELAY_NAME is cosmetic; don't stomp a user-set value.
	if os.Getenv("RELAY_NAME") == "" {
		if err := os.Setenv("RELAY_NAME", "playground"); err != nil {
			return err
		}
	}
	// Default the WebTransport origin policy so the browser UI (served on
	// UIAddr) can reach the relay (same host, different port — the relay URL is
	// derived per-request from the browser's Host). Respect an explicit value.
	if os.Getenv(cors.EnvVar) == "" {
		if err := os.Setenv(cors.EnvVar, cors.SameHost); err != nil {
			return err
		}
	}
	_ = os.Unsetenv("PEERS")
	return nil
}

// uiDisplayURL returns the http:// URL to print for the user. When the UI bind
// host is a wildcard (0.0.0.0 / [::] / ""), fall back to "localhost" so the
// printed URL is at least locally reachable; otherwise use the bind address.
func uiDisplayURL(uiAddr string) string {
	h, port, err := net.SplitHostPort(uiAddr)
	if err != nil {
		return "http://" + uiAddr
	}
	if h == "" || h == "0.0.0.0" || h == "::" {
		h = "localhost"
	}
	return "http://" + net.JoinHostPort(h, port)
}

// portOf extracts the port from a host:port address, returning "" if it can't.
func portOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i+1:]
		}
	}
	return ""
}
