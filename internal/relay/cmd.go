package relay

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/internal/cors"
	"github.com/qumo-dev/qumo/internal/envconfig"
	"github.com/qumo-dev/qumo/internal/gctune"
)

// sanitizeLog strips CR and LF from s to prevent log injection.
func sanitizeLog(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

// Run starts the MoQ relay server.
//
// Configuration is read from environment variables:
//
//	RELAY_ADDR                   - listen address (default: ":4433", dual-stack:
//	                               binds both IPv4 and IPv6 so `localhost` works
//	                               on hosts where it resolves to ::1)
//	CERT_FILE                    - TLS certificate file (default: "certs/server.crt")
//	KEY_FILE                     - TLS key file (default: "certs/server.key")
//	CA_FILE                      - PEM CA certificate (optional). A session
//	                               whose client certificate it verifies is a
//	                               trusted relay peer; relays this one dials are
//	                               also verified against it. Unset: no peers.
//	RELAY_NAME                   - node ID (default: "relay-" + hostname)
//	GROUP_CACHE_SIZE             - completed groups retained per track (default: 8)
//	FRAME_CAPACITY               - frame buffer size in bytes (default: 1500)
//	PEERS                        - comma-separated relays to dial (host:port);
//	                               each host is resolved to all its addresses
//	                               (e.g. "role-hub.qumo-relay.service.consul:4433")
//	CORS_ALLOWED_ORIGINS     - comma-separated WebTransport origins allowed to
//	                           connect (default: same-origin only; "*" allows any;
//	                           "same-host" allows any port on the request's host).
//	                           Set this when serving the UI from a different
//	                           origin than the relay (e.g. a Vite dev server).
func Run(args []string) error {
	// Execution modes are flags (discoverable, self-documenting); secrets and
	// deployment configuration stay env vars (see relay-config.example.env).
	flags, err := parseRelayArgs(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			relayUsage(os.Stdout)
			return nil
		}
		return err
	}

	gctune.Apply()

	addr := envconfig.String("RELAY_ADDR", ":4433")
	certFile := envconfig.String("CERT_FILE", "certs/server.crt")
	keyFile := envconfig.String("KEY_FILE", "certs/server.key")

	hostname, _ := os.Hostname()
	nodeID := envconfig.String("RELAY_NAME", "relay-"+hostname)

	groupCacheSize, err := envInt("GROUP_CACHE_SIZE", DefaultGroupCacheSize)
	if err != nil {
		return fmt.Errorf("invalid GROUP_CACHE_SIZE: %w", err)
	}
	frameCapacity, err := envInt("FRAME_CAPACITY", 1500)
	if err != nil {
		return fmt.Errorf("invalid FRAME_CAPACITY: %w", err)
	}

	var peers []Peer
	for _, p := range splitAddrList(os.Getenv("PEERS")) {
		peers = append(peers, Peer{Address: p})
	}

	tlsConfig, err := setupTLS(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("failed to setup TLS: %w", err)
	}

	// Peer trust: with CA_FILE, a client certificate is optional for everyone
	// (browsers present none) and verified when given; a verified one makes the
	// session a trusted relay peer. Without CA_FILE no session is a peer.
	caPEM, err := readCAFile(os.Getenv("CA_FILE"))
	if err != nil {
		return fmt.Errorf("failed to load CA_FILE: %w", err)
	}
	if caPEM != nil {
		clientCAs := x509.NewCertPool()
		clientCAs.AppendCertsFromPEM(caPEM)
		tlsConfig.ClientAuth = tls.VerifyClientCertIfGiven
		tlsConfig.ClientCAs = clientCAs
		slog.Info("relay: peering on: sessions with a client certificate verified against CA_FILE are trusted peers",
			"ca_file", os.Getenv("CA_FILE"))
	} else {
		slog.Info("relay: peering off (no CA_FILE): every inbound session is admitted like a client")
	}

	// Session admission (admit.go): the auth server beside this relay, or,
	// with no QUMO_AUTH_URL, auth off.
	authCfg := auth.LoadConfig()
	authorize := admitUnchecked
	var reportEnd func(context.Context, auth.Request) error // nil: auth off reports nothing
	authMode := "off (QUMO_AUTH_URL unset): every session is admitted unchecked"
	if authCfg.URL != "" {
		authClient, err := auth.NewClient(authCfg.URL)
		if err != nil {
			return fmt.Errorf("QUMO_AUTH_URL: %w", err)
		}
		authorize, reportEnd, authMode = authClient.Authorize, authClient.End, authCfg.URL
	} else {
		slog.Warn("relay: auth is off: QUMO_AUTH_URL is not set, so every session is admitted unchecked")
	}

	relayCfg := Config{
		NodeID:         nodeID,
		Role:           flags.Role,
		GroupCacheSize: groupCacheSize,
		FrameCapacity:  frameCapacity,
		Peers:          peers,
		NextSessionURI: os.Getenv("GOAWAY_REDIRECT_URI"),
	}

	// Setup signal handling for graceful shutdown
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Create relay server
	httpMux := http.NewServeMux()

	quicConfig := &quic.Config{
		Allow0RTT:                        true,
		EnableDatagrams:                  true,
		EnableStreamResetPartialDelivery: true,
		KeepAlivePeriod:                  10 * time.Second,
		MaxIdleTimeout:                   60 * time.Second,
		// MaxIncomingUniStreams/Streams left at quic-go defaults (~100). gomoqt's
		// OpenGroup(ctx) blocks on MAX_STREAMS as designed backpressure; the relay
		// passes a deadline-bearing context (openGroupTimeout) so a blocked open
		// drops the group (MoQ semi-reliable) rather than hanging the egress
		// goroutine and accumulating stream objects. Setting these to 1<<20 (as a
		// previous fix did) removes the backpressure entirely, causing unbounded
		// stream-object retention and a GC-driven degradation spiral.
	}

	// Dialer TLS: advertise only moqt ALPN for native QUIC peer connections.
	// The server TLS config advertises ["h3", "moqt"] to support both
	// WebTransport (browsers) and native QUIC (peer relays). If the dialer
	// sends both, TLS ALPN picks "h3" first → QPACK decompression failure.
	dialerTLS := tlsConfig.Clone()
	dialerTLS.NextProtos = []string{moqt.NextProtoMOQ}
	// Verify dialed relays against the system roots plus CA_FILE (a public
	// server certificate, or a private one under CA_FILE), and present this
	// relay's own certificate as the client certificate. ClientAuth/ClientCAs
	// are server-side settings.
	dialerTLS.ClientAuth = tls.NoClientCert
	dialerTLS.ClientCAs = nil
	if caPEM != nil {
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		roots.AppendCertsFromPEM(caPEM)
		dialerTLS.RootCAs = roots
	}

	// Override the QUIC listener's UDP receive buffer (SO_RCVBUF) to a
	// burst-safe size. RELAY_UDP_RCVBUF env var controls the value in bytes;
	// defaults to 256 KB (262144) which is well above the Windows default
	// (~8 KB) and matches Linux auto-tuning. Set to 0 to disable the override
	// and use the OS default.
	customLN := customQUICListener()
	moqtServer := &moqt.Server{
		Addr:               addr,
		TLSConfig:          tlsConfig,
		QUICConfig:         quicConfig,
		WebTransportServer: moqt.NewWebTransportServer(httpMux),
		NextSessionURI:     relayCfg.NextSessionURI,
	}
	if customLN != nil {
		moqtServer.ListenFunc = customLN
	}
	trackMux := moqt.NewTrackMux(moqt.NewHopID())
	relayServer := &Server{
		MOQServer: moqtServer,
		MOQDialer: &moqt.Dialer{
			TLSConfig:  dialerTLS,
			QUICConfig: quicConfig,
			OnGoaway:   handlePeerGoaway,
		},
		Config:         &relayCfg,
		TrackMux:       trackMux,
		AllowedOrigins: cors.LoadAllowed(),
		Authorize:      authorize,
		End:            reportEnd,
	}

	httpMux.HandleFunc("/", relayServer.HandleWebTransport)
	httpMux.HandleFunc("/health", relayServer.ServeHealth)
	httpMux.HandleFunc("/routes", relayServer.ServeStatus)
	httpMux.Handle("/metrics", promhttp.Handler())

	// /debug/stages exposes gomoqt's per-stage accept pipeline counters when the
	// instrumented gomoqt is linked (-tags instrument); the default build
	// registers a stub returning "{}". See debug_stages_{noop,instrument}.go.
	registerStagesDebug(httpMux, relayServer)

	// Optional net/http/pprof endpoints, off by default. pprof exposes runtime
	// internals (heap object graphs, goroutine stacks) so it is gated behind
	// RELAY_PPROF=1 and should only be enabled on a trusted/loopback interface.
	// Intended for capacity profiling under load (e.g. drive the relay to the
	// session ceiling via qumo loadgen and capture /debug/pprof/{heap,profile}).
	if os.Getenv("RELAY_PPROF") != "" {
		httpMux.HandleFunc("/debug/pprof/", pprof.Index)
		httpMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		httpMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		httpMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		httpMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		log.Printf("\t%-8s: pprof (RELAY_PPROF on)\n", "/debug/pprof/")
	}

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           httpMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("\t%-8s: %s\n", "Host", sanitizeLog(addr))
	log.Printf("\t%-8s: %s\n", "Node ID", sanitizeLog(relayCfg.NodeID))
	if relayCfg.Role != "" {
		log.Printf("\t%-8s: %s\n", "Role", sanitizeLog(relayCfg.Role))
	}
	log.Printf("\t%-8s: WebTransport endpoint\n", "/")
	log.Printf("\t%-8s: health probe\n", "/health")
	log.Printf("\t%-8s: Prometheus metrics\n", "/metrics")
	for _, p := range relayCfg.Peers {
		log.Printf("\t%-8s: %s\n", "Peer", sanitizeLog(p.Address))
	}
	log.Printf("\t%-8s: %s\n", "Auth", sanitizeLog(authMode))

	// Start peer connections in background
	go relayServer.ConnectPeers(ctx)

	// Delegate to testable helper that runs servers until ctx is cancelled
	if err := serveComponents(ctx, relayServer, httpServer, 10*time.Second); err != nil {
		slog.Error("serveComponents failed", "err", err)
		cancel()
		return err
	}

	return nil
}

// server is a minimal interface implemented by both *Server and
// *http.Server so we can unit-test the run/shutdown flow with fakes.
type server interface {
	ListenAndServe() error
	Shutdown(ctx context.Context) error
}

// serveComponents starts the provided servers and blocks until ctx is cancelled.
// It recovers panics from ListenAndServe goroutines, returns the first
// observed error, and performs a graceful shutdown of both servers.
//
// Design notes:
//   - serveComponents owns panic recovery and error reporting but does *not*
//     call the caller's cancel; the caller decides how to handle returned
//     errors (and may cancel the parent context).
//   - We use explicit Shutdown() calls because ListenAndServe blocks until the
//     server stops (it does not return on context cancellation by itself).
//   - This function intentionally keeps explicit control flow rather than
//     using errgroup so the shutdown ordering is clear and testable.
func serveComponents(ctx context.Context, relaySrv server, httpSrv server, shutdownTimeout time.Duration) error {
	// Create a derived cancellable context we can cancel when servers exit.
	derivedCtx, derivedCancel := context.WithCancel(ctx)
	defer derivedCancel()

	g, gctx := errgroup.WithContext(derivedCtx)

	g.Go(func() (retErr error) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in relay ListenAndServe", "panic", r)
				derivedCancel()
				retErr = fmt.Errorf("panic in relay ListenAndServe: %v", r)
			}
		}()

		if err := relaySrv.ListenAndServe(); err != nil {
			derivedCancel()
			return fmt.Errorf("relay ListenAndServe: %w", err)
		}
		derivedCancel()
		return nil
	})

	g.Go(func() (retErr error) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in HTTP ListenAndServe", "panic", r)
				derivedCancel()
				retErr = fmt.Errorf("panic in HTTP ListenAndServe: %v", r)
			}
		}()

		if err := httpSrv.ListenAndServe(); err != nil {
			if errors.Is(err, http.ErrServerClosed) {
				derivedCancel()
				return nil
			}
			derivedCancel()
			return fmt.Errorf("http ListenAndServe: %w", err)
		}
		derivedCancel()
		return nil
	})

	// Supervisor: when derived context is done, perform graceful shutdown.
	shutdownDone := make(chan struct{})
	go func() {
		<-gctx.Done()

		shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(gctx), shutdownTimeout)
		defer shutdownCancel()

		if err := relaySrv.Shutdown(shutdownCtx); err != nil {
			slog.Error("relay shutdown error", "err", err)
		}
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			slog.Error("HTTP server shutdown error", "err", err)
		}

		close(shutdownDone)
	}()

	// Wait for goroutines to finish; err will be first non-nil error (if any).
	err := g.Wait()

	// Ensure shutdown completed before returning.
	<-shutdownDone

	return err
}

func envInt(key string, defaultVal int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// readCAFile reads a PEM-encoded CA certificate file and checks it holds at
// least one certificate. Returns (nil, nil) when caFile is empty: peering off.
// CA_FILE must be a relative path with no path traversal components.
func readCAFile(caFile string) ([]byte, error) {
	if caFile == "" {
		return nil, nil
	}
	if filepath.IsAbs(caFile) {
		return nil, fmt.Errorf("CA_FILE must be a relative path")
	}
	caFile = filepath.Clean(caFile)
	if caFile == ".." || strings.HasPrefix(caFile, ".."+string(filepath.Separator)) || strings.Contains(caFile, string(filepath.Separator)+".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("CA_FILE must not contain path traversal")
	}
	pemData, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA file %q: %w", caFile, err)
	}
	if !x509.NewCertPool().AppendCertsFromPEM(pemData) {
		return nil, fmt.Errorf("no valid certificates in CA file %q", caFile)
	}
	return pemData, nil
}

func setupTLS(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load TLS certificates: %w", err)
	}

	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h3", moqt.NextProtoMOQ}, // HTTP/3 for WebTransport, MOQ native QUIC
	}, nil
}

// relayUsage writes the `qumo relay` flag summary to w. Flags cover execution
// modes only; everything else is configured via environment variables (see
// relay-config.example.env) to keep secrets out of argv and stay 12-factor
// friendly for container deployment.
func relayUsage(w io.Writer) {
	const help = `Usage: qumo relay [flags]

Start the MoQT relay server.

Flags:
  --role <hub|edge>  node topology role, logged for operator visibility only
                     (default: flat / single-node); connection topology is
                     controlled by PEERS, not this flag

All other configuration is via environment variables;
see relay-config.example.env for the full list.
`
	_, _ = io.WriteString(w, help)
}

// relayFlags holds parsed `qumo relay` execution-mode flags. Execution modes
// are flags; secrets and deployment configuration stay env (see
// relay-config.example.env). Add future runtime knobs (e.g. --log-level) here.
type relayFlags struct {
	// Role is an operator-facing label for this node's topology role: "hub"
	// (inter-region), "edge" (client-facing), or empty for a flat / single-node
	// relay. It is logged at startup for visibility only — it does not shape
	// connection behavior; that is controlled by PEERS.
	// Flag-only (no env equivalent) to avoid two sources of truth.
	Role string
}

// parseRelayArgs parses `qumo relay` flags into a relayFlags. flag.ErrHelp is
// returned as-is so Run can render usage and exit 0.
func parseRelayArgs(args []string) (relayFlags, error) {
	var f relayFlags
	fs := flag.NewFlagSet("qumo relay", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // Run owns help/usage rendering
	fs.StringVar(&f.Role, "role", "",
		`operator-facing topology label: "hub" (inter-region) or "edge" `+
			`(client-facing); empty (default) is a flat / single-node relay. `+
			`Logged for visibility only — does not affect connection behavior `+
			`(see PEERS).`)
	if err := fs.Parse(args); err != nil {
		return relayFlags{}, err
	}
	if fs.NArg() > 0 {
		return relayFlags{}, fmt.Errorf("unexpected argument: %s", fs.Arg(0))
	}
	return f, nil
}
