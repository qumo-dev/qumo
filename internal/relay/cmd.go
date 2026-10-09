package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
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
//	CA_FILE                      - PEM CA certificate (optional): the relay CA.
//	                               A session whose client certificate it
//	                               verifies is an internal client, or a relay
//	                               peer when the certificate carries the
//	                               peering name (peer_trust.go). Unset: no
//	                               session is either.
//	PEER_CERT_FILE, PEER_KEY_FILE - this relay's peer identity (optional), a
//	                               certificate CA_FILE issued for the peering
//	                               name; set together, and only with CA_FILE.
//	                               Needed to dial PEERS and to be dialed.
//	RELAY_NAME                   - node ID (default: "relay-" + hostname)
//	GROUP_CACHE_SIZE             - completed groups retained per track (default: 8)
//	FRAME_CAPACITY               - frame buffer size in bytes (default: 1500)
//	PEERS                        - comma-separated relays to dial (host:port);
//	                               each host is resolved to all its addresses
//	                               (e.g. "role-hub.qumo-relay.service.consul:4433").
//	                               Needs CA_FILE and the peer identity.
//	CORS_ALLOWED_ORIGINS     - comma-separated WebTransport origins allowed to
//	                           connect (default: same-origin only; "*" allows any;
//	                           "same-host" allows any port on the request's host).
//	                           Set this when serving the UI from a different
//	                           origin than the relay (e.g. a Vite dev server).
//	                           It applies to WebSocket upgrades too, where it is
//	                           the only check: browsers do not apply CORS to
//	                           WebSocket.
//	WS_ENABLE                - "1" to also take WebSocket upgrades on the
//	                           client endpoint and serve them as MoQ sessions
//	                           over QMux, for clients whose WebTransport does not
//	                           work (every browser on WebKit). They arrive on
//	                           RELAY_ADDR's TCP port, which is plain HTTP: put
//	                           TLS in front of it, or set WS_TLS_ADDR.
//	WS_TLS_ADDR              - TCP address (e.g. ":443") to take WebSocket
//	                           upgrades on with TLS, using CERT_FILE and
//	                           KEY_FILE. Implies WS_ENABLE. It must not be
//	                           RELAY_ADDR's port.
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

	// A relay still configured to ask an auth server must not start with
	// auth off instead.
	if os.Getenv("QUMO_AUTH_URL") != "" {
		return errors.New("QUMO_AUTH_URL is no longer supported: the relay verifies credentials itself; " +
			"unset it, and set QUMO_AUTH_KEYS to the key set")
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

	// Peer trust (peer_trust.go): a CA and this relay's own peer identity,
	// checked before anything is loaded so a settings mistake is what fails.
	trust, err := loadPeerTrust(os.Getenv("CA_FILE"), os.Getenv("PEER_CERT_FILE"), os.Getenv("PEER_KEY_FILE"), len(peers) > 0)
	if err != nil {
		return err
	}

	tlsConfig, err := setupTLS(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("failed to setup TLS: %w", err)
	}
	// The relay's certificate, for the WebSocket listener too: tlsConfig
	// itself goes on to carry the peer trust, which is QUIC's.
	certificates := tlsConfig.Certificates

	wsTLSAddr := os.Getenv("WS_TLS_ADDR")
	if err := checkWebSocketAddr(addr, wsTLSAddr); err != nil {
		return err
	}

	tlsConfig = trust.ServerTLS(tlsConfig)
	switch {
	case trust.Identity() != "":
		slog.Info("relay: peering on: sessions with a certificate from CA_FILE that carries the peering name are peers",
			"ca_file", os.Getenv("CA_FILE"), "identity", trust.Identity())
	case trust.HasCA():
		slog.Info("relay: internal clients on: sessions with a certificate from CA_FILE may subscribe; this relay is no peer (no PEER_CERT_FILE)",
			"ca_file", os.Getenv("CA_FILE"))
	default:
		slog.Info("relay: peering off (no CA_FILE): every inbound session is admitted like a client")
	}

	// Session admission (admit.go): the relay verifies credentials itself
	// against a key set (QUMO_AUTH_KEYS: a URL or a file), or, without one,
	// runs with auth off.
	verifierCfg := auth.VerifierConfig{
		Keys:      os.Getenv("QUMO_AUTH_KEYS"),
		KeysCache: os.Getenv("QUMO_AUTH_KEYS_CACHE"),
		UsageURL:  os.Getenv("QUMO_USAGE_URL"),
		Token:     os.Getenv("QUMO_RELAY_TOKEN"),
	}
	authorize := admitUnchecked
	var reportEnd func(context.Context, auth.Request) error // nil: auth off reports nothing
	var verifier *auth.Verifier
	if verifierCfg.Keys != "" {
		v, err := auth.NewVerifier(verifierCfg)
		if err != nil {
			return err
		}
		verifier = v
		authorize = v.Authorize
		reportEnd = v.End
	} else {
		slog.Warn("relay: auth is off: no key set (QUMO_AUTH_KEYS), so every session is admitted unchecked")
	}
	if verifierCfg.UsageURL != "" && verifier == nil {
		return errors.New("QUMO_USAGE_URL needs QUMO_AUTH_KEYS: usage is reported for the sessions the relay verifies")
	}

	relayCfg := Config{
		WebSocket:      wsTLSAddr != "" || envBool("WS_ENABLE"),
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

	// Dialing peers: the peer identity as the client certificate, the peering
	// name as the server name, the CA as the only root (peer_trust.go). The
	// dialer advertises only the moqt ALPN: with "h3" too, ALPN would pick
	// h3 and QPACK decompression fail. Without a peer identity there are no
	// PEERS to dial (loadPeerTrust), so the dialer never runs.
	var dialerTLS *tls.Config
	if trust.Identity() != "" {
		dialerTLS = trust.DialerTLS()
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
		Config:          &relayCfg,
		TrackMux:        trackMux,
		AllowedOrigins:  cors.LoadAllowed(),
		Authorize:       authorize,
		End:             reportEnd,
		PeerCertificate: trust.OwnCertificate(),
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

	// WebSocket with TLS: WebSocket upgrades alone, on a TCP port of their
	// own. The health, status and metrics routes stay on the plain HTTP
	// server.
	httpServers := servers{httpServer}
	if wsTLSAddr != "" {
		httpServers = append(httpServers, tlsServer{&http.Server{
			Addr:              wsTLSAddr,
			Handler:           http.HandlerFunc(relayServer.HandleWebSocket),
			TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, Certificates: certificates},
			ReadHeaderTimeout: 5 * time.Second,
		}})
	}

	log.Printf("\t%-8s: %s\n", "Host", sanitizeLog(addr))
	log.Printf("\t%-8s: %s\n", "Node ID", sanitizeLog(relayCfg.NodeID))
	if relayCfg.Role != "" {
		log.Printf("\t%-8s: %s\n", "Role", sanitizeLog(relayCfg.Role))
	}
	log.Printf("\t%-8s: WebTransport endpoint\n", "/")
	switch {
	case wsTLSAddr != "":
		log.Printf("\t%-8s: WebSocket (QMux) endpoint, also with TLS on %s\n", "/", sanitizeLog(wsTLSAddr))
	case relayCfg.WebSocket:
		log.Printf("\t%-8s: WebSocket (QMux) endpoint, plain HTTP: needs TLS in front\n", "/")
	}
	log.Printf("\t%-8s: health probe\n", "/health")
	log.Printf("\t%-8s: Prometheus metrics\n", "/metrics")
	for _, p := range relayCfg.Peers {
		log.Printf("\t%-8s: %s\n", "Peer", sanitizeLog(p.Address))
	}
	switch {
	case trust.Identity() != "":
		log.Printf("\t%-8s: %s, trusting %s\n", "Peering", sanitizeLog(trust.Identity()), sanitizeLog(os.Getenv("CA_FILE")))
	case trust.HasCA():
		log.Printf("\t%-8s: internal clients only, trusting %s (no PEER_CERT_FILE)\n", "Peering", sanitizeLog(os.Getenv("CA_FILE")))
	default:
		log.Printf("\t%-8s: off (no CA_FILE)\n", "Peering")
	}
	if verifier != nil {
		log.Printf("\t%-8s: key set %s\n", "Auth", sanitizeLog(verifier.Source()))
		if verifier.Reporting() {
			log.Printf("\t%-8s: %s\n", "Usage", sanitizeLog(verifierCfg.UsageURL))
		}
	} else {
		log.Printf("\t%-8s: off (no QUMO_AUTH_KEYS): every session is admitted unchecked\n", "Auth")
	}

	// The verifier refreshes its key set and sends usage until the relay has
	// stopped taking sessions, then sends what usage is left once more.
	verifierDone := make(chan struct{})
	stopVerifier := func() {}
	if verifier != nil {
		vctx, vcancel := context.WithCancel(context.WithoutCancel(ctx))
		stopVerifier = func() { vcancel(); <-verifierDone }
		go func() {
			defer close(verifierDone)
			verifier.Run(vctx)
		}()
	}
	defer stopVerifier()
	// Deferred after stopVerifier, so it runs before it: sessions report
	// their end after their connection closes, which can be after the
	// servers have shut down, and the last usage send must include them.
	defer relayServer.waitSessionEnds(endReportGrace)

	// Start peer connections in background
	go relayServer.ConnectPeers(ctx)

	// Delegate to testable helper that runs servers until ctx is cancelled
	if err := serveComponents(ctx, relayServer, httpServers, 10*time.Second); err != nil {
		slog.Error("serveComponents failed", "err", err)
		cancel()
		return err
	}

	return nil
}

// endReportGrace bounds how long shutdown waits for closed sessions to report
// their end before the verifier's last usage send.
const endReportGrace = 5 * time.Second

// server is a minimal interface implemented by both *Server and
// *http.Server so we can unit-test the run/shutdown flow with fakes.
type server interface {
	ListenAndServe() error
	Shutdown(ctx context.Context) error
}

// envBool reports whether the environment variable key is set to a true
// value: "1", "true", "yes" or "on", in any case.
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// checkWebSocketAddr reports a WS_TLS_ADDR the relay could not serve: one
// that is not an address, or that would bind the TCP port RELAY_ADDR's
// plain HTTP server already binds. An empty wsAddr is fine: there is no
// listener.
func checkWebSocketAddr(relayAddr, wsAddr string) error {
	if wsAddr == "" {
		return nil
	}
	wsHost, wsPort, err := net.SplitHostPort(wsAddr)
	if err != nil {
		return fmt.Errorf("invalid WS_TLS_ADDR %q: %w", wsAddr, err)
	}
	relayHost, relayPort, err := net.SplitHostPort(relayAddr)
	if err != nil {
		// RELAY_ADDR's own check is the listener's.
		return nil
	}
	// An empty host binds every address, so it collides with any host.
	if wsPort == relayPort && (wsHost == relayHost || wsHost == "" || relayHost == "") {
		return fmt.Errorf("WS_TLS_ADDR %q is RELAY_ADDR's TCP port, which serves plain HTTP (health, metrics): give WS_TLS_ADDR a port of its own", wsAddr)
	}
	return nil
}

// tlsServer is an HTTP server that serves TLS with the certificates of its
// TLSConfig.
type tlsServer struct {
	*http.Server
}

func (s tlsServer) ListenAndServe() error {
	return s.ListenAndServeTLS("", "")
}

// servers runs several servers as one: ListenAndServe returns when the
// first of them does, and Shutdown shuts them all down.
type servers []server

func (ss servers) ListenAndServe() error {
	errs := make(chan error, len(ss))
	for _, s := range ss {
		go func() {
			// A panic ends this server like an error, so that the caller
			// shuts the others down instead of the process dying.
			defer func() {
				if r := recover(); r != nil {
					errs <- fmt.Errorf("panic in ListenAndServe: %v", r)
				}
			}()
			errs <- s.ListenAndServe()
		}()
	}
	// The others end at Shutdown, which the caller runs once this returns.
	return <-errs
}

func (ss servers) Shutdown(ctx context.Context) error {
	var errs []error
	for _, s := range ss {
		errs = append(errs, s.Shutdown(ctx))
	}
	return errors.Join(errs...)
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
