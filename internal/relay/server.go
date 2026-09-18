package relay

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/cors"
)

// authTrackName is the well-known MoQ track name on which publishers send
// their JWT credential. The relay subscribes to this track at ANNOUNCE time.
const authTrackName moqt.TrackName = "auth"

type Server struct {
	// MOQServer is the underlying MoQT server. The caller is responsible for
	// setting Addr, TLSConfig (must include all accepted ALPNs, e.g. ["h3", "moqt"]),
	// QUICConfig, and WebTransportServer. Handler and TrackMux are wired by init().
	MOQServer *moqt.Server
	// MOQDialer is used for outbound peer connections. The caller must set
	// TLSConfig with NextProtos: []string{moqt.NextProtoMOQ} only, so that
	// ALPN negotiation does not accidentally select "h3".
	MOQDialer *moqt.Dialer
	Config    *Config
	TrackMux  *moqt.TrackMux

	// AllowedOrigins is the list of WebTransport origins the browser-facing
	// handler accepts (CSWT mitigation). nil/empty = same-origin only (secure
	// default); "*" allows any. Populated from CORS_ALLOWED_ORIGINS by the
	// relay command. See internal/cors.
	AllowedOrigins []string

	// credentialClient is non-nil when QUMO_CREDENTIAL_URL is configured.
	// It handles credential introspection and usage reporting.
	credentialClient *CredentialClient
	// meter drives periodic and final usage reporting for metered sessions.
	// It is non-nil exactly when credentialClient is non-nil.
	meter *Meter

	// framePool recycles frame buffers for track distributors and the auth
	// track read; sized from Config.FrameCapacity in init() (falling back to
	// DefaultFramePool when unset, so a minimally-constructed Server still works).
	framePool *FramePool

	webtransportHandler *moqt.WebTransportHandler
	statusHandler       *statusHandler
	initOnce            sync.Once

	// sampler is the single server-wide stats sampler. It replaces the former
	// per-connection, per-session, and per-track poller goroutines with one
	// registry-sweeping loop, eliminating their GC stack-scan cost at high
	// fan-out. Created and started in init(); stopped by samplerCancel.
	sampler       *statsSampler
	samplerCancel context.CancelFunc

	// connectedMu guards connected, which tracks peer addresses already dialing
	// or connected to prevent duplicate maintainPeer goroutines.
	connectedMu sync.Mutex
	connected   map[string]struct{}

	// routeMu guards alternates: per-BroadcastPath route-election losers that
	// are retained (not cancelled) so they can be promoted if the active route's
	// announcement ends. See retainRoute / promoteAlternate.
	routeMu    sync.Mutex
	alternates map[moqt.BroadcastPath]*alternate

	// pathStatusMu guards pathStatus, the deploy-facing snapshot of active
	// broadcast paths and their route metrics.
	pathStatusMu sync.Mutex
	pathStatus   map[moqt.BroadcastPath]overlayPathStatus
}

func (s *Server) ServeHealth(w http.ResponseWriter, r *http.Request) {
	s.init()
	if s.statusHandler == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	s.statusHandler.ServeHTTP(w, r)
}

func (s *Server) ServeStatus(w http.ResponseWriter, r *http.Request) {
	s.init()
	if s.statusHandler == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	s.statusHandler.ServeStatus(w, r)
}

func (s *Server) HandleWebTransport(w http.ResponseWriter, r *http.Request) {
	s.init()
	if s.webtransportHandler == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	s.webtransportHandler.ServeHTTP(w, r)
}

func (s *Server) init() {
	s.initOnce.Do(func() {
		// Single server-wide stats sampler. Started here (once) and stopped in
		// Close/Shutdown via samplerCancel. Uses a Background-derived context
		// because the relay has no server-lifetime context of its own; the
		// cancel is the sole stop signal.
		s.sampler = &statsSampler{}
		samplerCtx, cancel := context.WithCancel(context.Background())
		s.samplerCancel = cancel
		go s.sampler.run(samplerCtx)

		if s.TrackMux == nil {
			s.TrackMux = moqt.NewTrackMux(0)
		}

		if s.statusHandler == nil {
			s.statusHandler = newStatusHandler()
		}
		s.statusHandler.server = s
		if s.pathStatus == nil {
			s.pathStatus = make(map[moqt.BroadcastPath]overlayPathStatus)
		}

		// Wire relay-specific fields into the caller-provided MoQServer.
		if s.MOQServer.Handler != nil {
			slog.Warn("relay.Server: overriding MOQServer.Handler set by caller")
		}
		// Native QUIC connections are always relay peers (ALPN "moqt").
		// WebTransport connections (ALPN "h3") are publisher/browser sessions
		// that require credential auth when the credential client is configured.
		s.MOQServer.Handler = moqt.HandleFunc(s.relayPeer)
		if s.MOQServer.TrackMux != nil {
			slog.Warn("relay.Server: overriding MOQServer.TrackMux set by caller")
		}
		s.MOQServer.TrackMux = s.TrackMux

		s.webtransportHandler = &moqt.WebTransportHandler{
			TrackMux:    s.TrackMux,
			Handler:     moqt.HandleFunc(s.Relay),
			Logger:      s.MOQServer.Logger,
			CheckOrigin: cors.NewChecker(s.AllowedOrigins),
		}

		// ConnContext intercepts each accepted QUIC connection before the MOQ
		// handshake. For native QUIC connections the underlying type satisfies
		// connStatsProvider, so we launch a polling goroutine to collect
		// connection-level stats (RTT, packet loss). WebTransport connections
		// do not satisfy the interface and are silently skipped.
		s.MOQServer.ConnContext = func(ctx context.Context, conn moqt.StreamConn) context.Context {
			if provider, ok := conn.(connStatsProvider); ok {
				addr := conn.RemoteAddr().String()
				s.sampler.addConn(addr, provider)
				sampleConnStats(provider, addr) // immediate first sample
				context.AfterFunc(conn.Context(), func() { s.sampler.removeConn(addr) })
			}
			return ctx
		}

		// Invariant: meter must be set whenever credentialClient is set.
		// A manually-constructed Server that sets credentialClient without meter
		// would panic later when the first metered announcement is accepted.
		if s.credentialClient != nil && s.meter == nil {
			panic("relay.Server: meter must be non-nil when credentialClient is set")
		}

		// Resolve the per-node frame pool from Config.FrameCapacity. A caller
		// that leaves it unset (≤0) reuses the package DefaultFramePool; an
		// explicit value mints a right-sized pool shared across tracks.
		if s.framePool == nil {
			s.framePool = resolveFramePool(s.Config)
		}

		s.connectedMu.Lock()
		if s.connected == nil {
			s.connected = make(map[string]struct{})
		}
		s.connectedMu.Unlock()

		if s.alternates == nil {
			s.alternates = make(map[moqt.BroadcastPath]*alternate)
		}
	})
}

// resolveFramePool selects the per-node frame pool: a dedicated pool sized by
// cfg.FrameCapacity when set (>0), otherwise the shared DefaultFramePool. nil
// cfg is treated as unset so a minimally-constructed Server still works.
func resolveFramePool(cfg *Config) *FramePool {
	if cfg != nil && cfg.FrameCapacity > 0 {
		return NewFramePool(cfg.FrameCapacity)
	}
	return DefaultFramePool
}

func (s *Server) setPathStatus(path moqt.BroadcastPath, stats RouteStats, source string, handler *relayHandler) {
	if s == nil {
		return
	}
	s.pathStatusMu.Lock()
	defer s.pathStatusMu.Unlock()
	if s.pathStatus == nil {
		s.pathStatus = make(map[moqt.BroadcastPath]overlayPathStatus)
	}
	now := time.Now()
	s.pathStatus[path] = overlayPathStatus{
		Path:        path.String(),
		Active:      true,
		AnnouncedAt: now,
		Hops:        stats.Hops,
		RTTMs:       stats.RTT.Milliseconds(),
		BitrateBps:  stats.EstimatedBitrate,
		Source:      source,
		LastUpdated: now,
		handler:     handler,
	}
}

func (s *Server) clearPathStatus(path moqt.BroadcastPath, handler *relayHandler) {
	if s == nil {
		return
	}
	s.pathStatusMu.Lock()
	defer s.pathStatusMu.Unlock()
	if s.pathStatus == nil {
		return
	}
	if current, ok := s.pathStatus[path]; ok && current.handler == handler {
		delete(s.pathStatus, path)
	}
}

// ListenAndServe starts the relay server.
func (s *Server) ListenAndServe() error {
	if s.MOQServer == nil {
		panic("relay.Server: MoQServer is required")
	}
	if s.MOQDialer == nil {
		panic("relay.Server: MoQDialer is required")
	}

	s.init()

	// Start server - this will block until server closes
	return s.MOQServer.ListenAndServe()
}

func (s *Server) Close() error {
	if s.samplerCancel != nil {
		s.samplerCancel()
	}
	if s.MOQServer != nil {
		_ = s.MOQServer.Close()
	}

	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.samplerCancel != nil {
		s.samplerCancel()
	}
	if s.MOQServer != nil {
		return s.MOQServer.Shutdown(ctx)
	}

	return nil
}

// ConnectPeers dials configured peer relays and upstream addresses, discovering
// their announcements via ANNOUNCE_PLEASE. Received announcements are registered
// on the local TrackMux so that subscribers can transparently access remote content.
// It blocks until ctx is cancelled.
func (s *Server) ConnectPeers(ctx context.Context) {
	s.init()
	var wg sync.WaitGroup

	dialPeer := func(addr string) {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			return
		}
		if !s.markConnected(addr) {
			return
		}
		wg.Go(func() {
			s.maintainPeer(ctx, Peer{Address: addr})
			s.markUnconnected(addr)
		})
	}

	// Static peers from config.
	for _, peer := range s.Config.Peers {
		dialPeer(peer.Address)
	}

	// Upstream relay address (e.g. role-hub.qumo-relay.service.consul:4433).
	// Used by edge relays to connect upstream, or any relay hierarchy.
	// When a hostname is provided (e.g. Consul DNS), all resolved A/AAAA records
	// are connected to so the edge acts as a multi-homed L7 load balancer.
	if s.Config.UpstreamAddr != "" {
		for _, u := range resolveUpstreamAddrs(ctx, s.Config.UpstreamAddr) {
			dialPeer(u)
		}
	}

	wg.Wait()
}

// resolveUpstreamAddrs resolves raw upstream address entries (supporting single DNS
// name with multiple A/AAAA records as well as comma-separated addresses).
// For each host:port, if host is not an IP, it attempts net.DefaultResolver.LookupIPAddr
// and returns ip:port for every resolved address. If resolution fails or yields no IPs,
// it falls back to the original entry.
func resolveUpstreamAddrs(ctx context.Context, raw string) []string {
	var results []string
	for _, entry := range splitAddrList(raw) {
		host, port, err := net.SplitHostPort(entry)
		if err != nil {
			// No port or unparseable host:port (e.g. invalid string) - keep as-is.
			results = append(results, entry)
			continue
		}
		if ip := net.ParseIP(host); ip != nil {
			// Already an IP address.
			results = append(results, entry)
			continue
		}

		lookupCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		ips, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
		cancel()

		if err != nil || len(ips) == 0 {
			// Fallback to original host:port if DNS lookup fails.
			results = append(results, entry)
			continue
		}

		for _, ip := range ips {
			results = append(results, net.JoinHostPort(ip.IP.String(), port))
		}
	}
	return results
}

// isConnected reports whether addr is currently in the connected set.
// It is safe for concurrent use.
func (s *Server) isConnected(addr string) bool {
	s.connectedMu.Lock()
	defer s.connectedMu.Unlock()
	if s.connected == nil {
		return false
	}
	_, ok := s.connected[addr]
	return ok
}

// markConnected records addr as connected and returns true if it was not already present.
// It is safe for concurrent use.
func (s *Server) markConnected(addr string) bool {
	s.connectedMu.Lock()
	defer s.connectedMu.Unlock()
	if s.connected == nil {
		s.connected = make(map[string]struct{})
	}
	if _, ok := s.connected[addr]; ok {
		return false
	}
	s.connected[addr] = struct{}{}
	metricPeersConnected.Inc()
	return true
}

// markUnconnected removes addr from the connected set.
// It is safe for concurrent use.
func (s *Server) markUnconnected(addr string) {
	s.connectedMu.Lock()
	defer s.connectedMu.Unlock()
	if s.connected == nil {
		return
	}
	delete(s.connected, addr)
	metricPeersConnected.Dec()
	// The per-addr session RTT/bitrate series are reaped by the stats sampler
	// when the peer session deregisters (serveSession's removeSession). Deleting
	// them here as well would race the sampler's Set writes (resurrecting the
	// series), so peer-metric cleanup is left to the sampler.
}

func (s *Server) maintainPeer(ctx context.Context, peer Peer) {
	var backoff = DialBackoff{Base: 1 * time.Second, Max: 30 * time.Second}

	for {
		if ctx.Err() != nil {
			return
		}

		sess, err := s.MOQDialer.DialQUIC(ctx, peer.Address, "", s.TrackMux)
		if err != nil {
			metricPeerDialAttempts.WithLabelValues(peer.Address, "error").Inc()
			metricDialRetriesTotal.WithLabelValues(peer.Address).Inc()
			slog.Warn("failed to dial peer", "address", peer.Address, "error", err,
				"retry_attempt", backoff.Attempts()+1)
			if !backoff.Wait(ctx) {
				return
			}
			continue
		}
		metricPeerDialAttempts.WithLabelValues(peer.Address, "ok").Inc()
		backoff.Reset()

		// Serve the current session, then on disconnect attempt an immediate
		// reconnect with jitter. On success, loop back to serve the new session;
		// on failure, break to the outer retry loop with exponential backoff.
		for {
			s.relayPeer(sess)

			<-sess.Context().Done()

			slog.Info("peer disconnected", "address", peer.Address)

			// Attempt one immediate reconnect with a small random jitter to
			// spread out synchronized reconnects. The jitter prevents a
			// thundering herd when many peers disconnect simultaneously.
			if !jitterDelay(ctx, 100*time.Millisecond) {
				return
			}
			sess, err = s.MOQDialer.DialQUIC(ctx, peer.Address, "", s.TrackMux)
			if err != nil {
				metricPeerDialAttempts.WithLabelValues(peer.Address, "error").Inc()
				metricDialRetriesTotal.WithLabelValues(peer.Address).Inc()
				slog.Warn("failed to dial peer", "address", peer.Address, "error", err,
					"retry_attempt", backoff.Attempts()+1)
				break
			}
			metricPeerDialAttempts.WithLabelValues(peer.Address, "ok").Inc()
			backoff.Reset()
		}

		if !backoff.Wait(ctx) {
			return
		}
	}
}

// Relay handles inbound WebTransport sessions (publishers and browser clients).
// When a backend client is configured, each announced broadcast path is
// authenticated via a JWT read from the "auth" MoQ track before being accepted.
func (s *Server) Relay(sess *moqt.Session) {
	s.serveSession(sess, true)
}

// relayPeer handles native QUIC sessions from trusted relay peers.
// These sessions are authenticated at the transport layer (mTLS) and bypass
// the per-announcement JWT credential check.
func (s *Server) relayPeer(sess *moqt.Session) {
	s.serveSession(sess, false)
}

// serveSession is the shared core for Relay and relayPeer.
// requireAuth=true enables per-announcement JWT authentication (publisher path).
func (s *Server) serveSession(sess *moqt.Session, requireAuth bool) {
	s.init()
	defer sess.CloseWithError(moqt.NoError, moqt.NoError.String())

	metricSessionsActive.Inc()
	defer metricSessionsActive.Dec()

	addr := sess.RemoteAddr().String()
	s.sampler.addSession(addr, sess)
	sampleSessionStats(sess, addr) // immediate first sample
	defer s.sampler.removeSession(addr)

	slog.Info("relay: new session", "remote", addr, "peer", !requireAuth)

	announced, err := sess.AcceptAnnounce("/")
	if err != nil {
		slog.Warn("failed to accept announcement", "error", err)
		return
	}
	for {
		ann, err := announced.ReceiveAnnouncement(sess.Context())
		if err != nil {
			slog.Warn("relay: announcements loop ended",
				"remote", sess.RemoteAddr(),
				"error", err,
				"reader_ctx_err", announced.Context().Err(),
				"sess_ctx_err", sess.Context().Err())
			return
		}

		slog.Debug("relay: received announcement",
			"node", s.Config.NodeID,
			"broadcast_path", ann.BroadcastPath(),
			"hops", len(ann.HopIDs()),
			"remote", sess.RemoteAddr(),
		)

		// Authenticate publisher announcements when the credential client is configured.
		var broadSess *broadcastSession
		if requireAuth && s.credentialClient != nil {
			broadSess, err = s.authenticateAnnouncement(sess.Context(), sess, ann)
			if err != nil {
				// MoQ has no per-announcement error response, so the publisher
				// receives no explicit rejection — the ANNOUNCE is simply not
				// mirrored into the TrackMux.
				slog.Warn("relay: announcement rejected: credential check failed",
					"broadcast_path", ann.BroadcastPath(),
					"error", err)
				continue
			}
		}

		handler := newRelayHandler(ann, sess, s.Config.NodeID, broadSess,
			s.Config.GroupCacheSize, s.framePool, s.sampler)

		slog.Debug("relay: created relayHandler",
			"node", s.Config.NodeID,
			"broadcast_path", ann.BroadcastPath(),
			"active", ann.IsActive(),
		)

		// Route selection: only replace an existing active handler if the new
		// route is strictly better. The decision and the TrackMux install are
		// performed under routeMu so they are atomic w.r.t. promoteAlternate
		// (and w.r.t. a concurrent election on another session): a promotion can
		// never clobber a freshly-elected route, nor vice versa.
		s.routeMu.Lock()
		rejected := false
		candidateStats := handler.RouteStats()
		if _, existing := s.TrackMux.TrackHandler(ann.BroadcastPath()); existing != nil {
			if rr, ok := existing.(RouteReporter); ok {
				currentStats := rr.RouteStats()
				decision := compareRoutes(candidateStats, currentStats)
				if !decision.accepted() {
					metricRouteRejections.WithLabelValues(decision.String()).Inc()
					slog.Debug("relay: route rejected",
						"node", s.Config.NodeID,
						"broadcast_path", ann.BroadcastPath(),
						"reason", decision.String(),
						"current_hops", currentStats.Hops,
						"current_rtt_ms", currentStats.RTT.Milliseconds(),
						"current_bitrate_bps", currentStats.EstimatedBitrate,
						"candidate_hops", candidateStats.Hops,
						"candidate_rtt_ms", candidateStats.RTT.Milliseconds(),
						"candidate_bitrate_bps", candidateStats.EstimatedBitrate,
					)
					// Retain as an alternate instead of discarding: if the active
					// route's announcement later ends (e.g. the publisher moved and
					// the incumbent publication is retracted shortly after this one
					// was rejected), it is promoted so subscribers are not left
					// stranded. See retainRoute / promoteAlternate.
					s.retainRouteLocked(handler)
					rejected = true
				} else {
					metricRouteReplacements.WithLabelValues(decision.String()).Inc()
					slog.Info("relay: route replaced",
						"node", s.Config.NodeID,
						"broadcast_path", ann.BroadcastPath(),
						"reason", decision.String(),
						"displaced_hops", currentStats.Hops,
						"displaced_rtt_ms", currentStats.RTT.Milliseconds(),
						"displaced_bitrate_bps", currentStats.EstimatedBitrate,
						"candidate_hops", candidateStats.Hops,
						"candidate_rtt_ms", candidateStats.RTT.Milliseconds(),
						"candidate_bitrate_bps", candidateStats.EstimatedBitrate,
					)
					// Gracefully drain the displaced handler.
					if dr, ok := existing.(Drainable); ok {
						dr.Drain(DrainTimeout)
					}
				}
			}
		}
		if !rejected {
			slog.Info("relay: route accepted (new broadcast)",
				"node", s.Config.NodeID,
				"broadcast_path", ann.BroadcastPath(),
				"hops", candidateStats.Hops,
				"rtt_ms", candidateStats.RTT.Milliseconds(),
				"bitrate_bps", candidateStats.EstimatedBitrate,
			)
			s.installRoute(handler)
		}
		s.routeMu.Unlock()
		if rejected {
			continue
		}
	}
}

// installRoute wires a winning relayHandler as the active route for its
// broadcast path: metric accounting, session-end cancellation, optional meter
// registration, and registration in the TrackMux. It also schedules recovery —
// when this route's announcement ends, any retained alternate is promoted.
func (s *Server) installRoute(h *relayHandler) {
	slog.Debug("relay: installing route",
		"node", s.Config.NodeID,
		"broadcast_path", h.announcement.BroadcastPath(),
	)

	// Track the broadcast route and release it when the handler's context is
	// cancelled (covers both normal session end and drain expiry).
	metricBroadcastsActive.Inc()
	context.AfterFunc(h.ctx, func() { metricBroadcastsActive.Dec() })

	// Session-end cleanup of the handler's child context is registered in
	// newRelayHandler (where the session is guaranteed non-nil), not here.

	source := ""
	if h.session != nil && h.session.RemoteAddr() != nil {
		source = h.session.RemoteAddr().String()
	}
	s.setPathStatus(h.announcement.BroadcastPath(), h.RouteStats(), source, h)

	// Register the broadcast session with the meter so usage is reported
	// periodically and on session close.
	if h.broadSession != nil {
		s.meter.Register(h.broadSession)
		context.AfterFunc(h.ctx, func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s.meter.Deregister(shutdownCtx, h.broadSession)
		})
	}

	// Recovery on incumbent-end: promote a retained alternate for this path
	// when this route's announcement ends, so the path is not left stranded.
	// Run asynchronously because Announcement.end() invokes AfterFunc callbacks
	// inline; promotion re-enters the TrackMux and must run only after end()
	// (and TrackMux's own removal handler) has fully completed.
	h.announcement.AfterFunc(func() {
		s.clearPathStatus(h.announcement.BroadcastPath(), h)
		go s.promoteAlternate(h.announcement.BroadcastPath())
	})

	s.TrackMux.Announce(h.announcement, h)
}

// alternate is a retained fallback route: a route-election loser kept alive so
// it can be promoted if the active route's announcement ends. stop deregisters
// its discardAlternate callback, so promotion/replacement can remove the
// callback instead of leaving it to fire as a no-op later.
type alternate struct {
	handler *relayHandler
	stop    func() bool
}

// retainRoute keeps a route-election loser alive as the alternate for its
// broadcast path instead of cancelling it. At most one alternate per path is
// retained, and it is the BEST seen (by compareRoutes), not merely the latest.
// Promotion only ever fires on a definitive announcement-end, so this
// introduces no route oscillation. Public wrapper; callers already holding
// routeMu (serveSession's election) use retainRouteLocked.
func (s *Server) retainRoute(h *relayHandler) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	s.retainRouteLocked(h)
}

// retainRouteLocked is retainRoute assuming routeMu is held. Because the whole
// body runs under routeMu, discardAlternate (which needs routeMu) cannot fire
// mid-body — so the alternate can be stored before its cleanup AfterFunc is
// registered without risk of stranding, and alt.stop can be assigned without
// a second critical section.
func (s *Server) retainRouteLocked(h *relayHandler) {
	// Never retain an already-dead handler.
	if !h.announcement.IsActive() || h.ctx.Err() != nil {
		h.cancel()
		return
	}
	path := h.announcement.BroadcastPath()

	// Keep the best alternate per path: if the existing alternate is at least
	// as good as the new one, keep it and drop the new one. Otherwise the new
	// one replaces the old — so promotion can never install a strictly worse
	// route than one the relay had already accepted and discarded.
	if prev := s.alternates[path]; prev != nil {
		if !compareRoutes(h.RouteStats(), prev.handler.RouteStats()).accepted() {
			h.cancel()
			return
		}
		if prev.stop != nil {
			prev.stop()
		}
		prev.handler.cancel()
		metricRelayRoutesRetained.Dec()
	}

	// Store first, then register the cleanup AfterFunc. If the announcement
	// ended between the liveness check above and here, AfterFunc fires
	// discardAlternate asynchronously; that goroutine blocks on routeMu (held
	// here) and, once released, finds h in the map and removes it. No IsActive
	// re-check is needed.
	alt := &alternate{handler: h}
	s.alternates[path] = alt
	metricRelayRoutesRetained.Inc()
	alt.stop = h.announcement.AfterFunc(func() {
		s.discardAlternate(path, h)
	})
}

// discardAlternate removes h from the retained alternates for path if it is
// still the retained alternate, then cancels it. Idempotent; a no-op if h has
// already been replaced or promoted.
func (s *Server) discardAlternate(path moqt.BroadcastPath, h *relayHandler) {
	s.routeMu.Lock()
	alt := s.alternates[path]
	if alt != nil && alt.handler == h {
		delete(s.alternates, path)
	} else {
		alt = nil
	}
	s.routeMu.Unlock()
	if alt != nil {
		metricRelayRoutesRetained.Dec()
		h.cancel()
	}
}

// promoteAlternate is invoked when the active route for a path ends. If a
// retained alternate is still live, it is installed as the new active route;
// otherwise the path is left unoccupied until a new announcement arrives.
//
// The whole pop + clobber-guard + install runs under routeMu, so it is atomic
// w.r.t. serveSession's election+install: the guard's read of TrackMux and the
// subsequent Announce cannot be interleaved by a concurrent election.
func (s *Server) promoteAlternate(path moqt.BroadcastPath) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()

	alt := s.alternates[path]
	if alt == nil {
		return
	}
	delete(s.alternates, path)
	metricRelayRoutesRetained.Dec()
	h := alt.handler

	// Deregister the discardAlternate callback (no-op if the announcement
	// already ended and it has fired).
	if alt.stop != nil {
		alt.stop()
	}

	if !h.announcement.IsActive() || h.ctx.Err() != nil {
		// The alternate died while retained; just release it.
		h.cancel()
		return
	}

	// Clobber guard: only promote into an empty or dead slot. Displacing the
	// incumbent ends its Announcement, which is what fires this promotion — so
	// the slot now holds the displacer (the route that just won election).
	// Promoting the alternate over it would subvert route selection. This check
	// is atomic with the install below because routeMu is held throughout.
	if ann, existing := s.TrackMux.TrackHandler(path); ann != nil {
		if rr, ok := existing.(RouteReporter); ok {
			if st := rr.RouteStats(); st.Alive {
				// Slot holds a live route; discard the alternate.
				h.cancel()
				return
			}
		} else {
			// Unknown handler type; don't clobber it.
			h.cancel()
			return
		}
	}

	metricRelayRoutePromotions.Inc()
	slog.Info("relay: promoting retained route after incumbent ended",
		"broadcast_path", path)
	s.installRoute(h)
}

// handlePeerGoaway is the Dialer.OnGoaway callback: an upstream peer relay sent
// GOAWAY (a migration/drain hint). Route/subscription migration is the primary
// mobility mechanism; GOAWAY is observed and surfaced to qumo-deploy via metrics.
// Automatic re-dial to the new URI is future work (gomoqt delivers OnGoaway
// without identifying which peer session originated it).
func handlePeerGoaway(newSessionURI string) {
	redirect := "absent"
	if newSessionURI != "" {
		redirect = "present"
	}
	metricPeerGoawayReceived.WithLabelValues(redirect).Inc()
	slog.Info("relay: upstream peer sent GOAWAY", "new_session_uri", newSessionURI)
}

// authenticateAnnouncement subscribes to the "auth" track on the announced
// broadcast path, reads the JWT from the first frame, and introspects it
// against the credential introspection endpoint.
//
// Publisher-side contract: the publisher must serve a single-group track named
// "auth" on the announced broadcast path. The group must contain at least one
// frame whose payload is the raw JWT bytes (no framing). The relay expects the
// complete JWT to arrive within the 5-second authCtx deadline.
//
// Returns the minted broadcastSession on success, or an error if authentication
// fails (missing track, empty JWT, or invalid/expired credential).
func (s *Server) authenticateAnnouncement(ctx context.Context, sess *moqt.Session, ann *moqt.Announcement) (*broadcastSession, error) {
	authCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	reader, err := sess.Subscribe(authCtx, ann.BroadcastPath(), authTrackName, nil)
	if err != nil {
		return nil, fmt.Errorf("subscribe auth track: %w", err)
	}

	gr, err := reader.AcceptGroup(authCtx)
	if err != nil {
		return nil, fmt.Errorf("accept auth group: %w", err)
	}

	buf := s.framePool.Get()
	defer s.framePool.Put(buf)

	var jwtBuf bytes.Buffer
	for frame := range gr.Frames(buf) {
		jwtBuf.Write(frame.Body())
	}
	if jwtBuf.Len() == 0 {
		return nil, fmt.Errorf("auth track: empty JWT")
	}
	jwt := jwtBuf.String()

	result, err := s.credentialClient.Introspect(authCtx, jwt)
	if err != nil {
		return nil, fmt.Errorf("introspect: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("credential rejected by backend")
	}

	return newBroadcastSession(result.TokenID), nil
}
