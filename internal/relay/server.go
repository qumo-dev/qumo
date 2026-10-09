package relay

import (
	"context"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/internal/cors"
)

type Server struct {
	// MOQServer is the underlying MoQT server. The caller is responsible for
	// setting Addr, TLSConfig (must include all accepted ALPNs, e.g. ["h3", "moqt"]),
	// QUICConfig, and WebTransportServer. Handler and TrackMux are wired by init().
	MOQServer *moqt.Server
	// MOQDialer is used for outbound peer connections. The caller must set
	// TLSConfig with NextProtos: []string{moqt.NextProtoMOQ} only, so that
	// ALPN negotiation does not accidentally select "h3"; the relay command
	// sets (*PeerTrust).DialerTLS, which also names PeeringName.
	MOQDialer *moqt.Dialer
	// PeerCertificate is this relay's own peer certificate (DER), or nil
	// without a peer identity. A dialed peer that presents it is this relay
	// itself, reached through a name in Config.Peers that resolves to it
	// too: that session is dropped and not retried. So is a session with another relay holding the same
	// certificate, which nothing tells apart from this one: every relay
	// needs a peer certificate of its own.
	PeerCertificate []byte
	Config          *Config
	TrackMux        *moqt.TrackMux

	// AllowedOrigins is the list of WebTransport origins the browser-facing
	// handler accepts (CSWT mitigation). nil/empty = same-origin only (secure
	// default); "*" allows any. Populated from CORS_ALLOWED_ORIGINS by the
	// relay command. See internal/cors.
	AllowedOrigins []string

	// Authorize decides whether a client session may start, and returns its
	// grant (admit.go). The relay command sets it to the Verifier's
	// (QUMO_AUTH_KEYS), or, with auth off, to a function that admits every
	// session unchecked. Other code in this module that builds a
	// Server, such as the black-box tests in internal/integration, supplies
	// its own. A nil grant with a nil error admits the session unchecked;
	// an auth.RefusedError refuses it with its status; any other error
	// refuses it as unavailable.
	//
	// A nil Authorize refuses every client session and logs why: a Server
	// never runs open by omission. Relay peers and internal clients
	// (peer_trust.go) are never asked.
	Authorize func(ctx context.Context, req auth.Request) (*auth.Grant, error)

	// End reports the end of a checked session, with its final byte totals:
	// the Verifier's End. Nil reports nothing, as with auth off. It is
	// called after the session has closed, so it can't hold a session open.
	End func(ctx context.Context, req auth.Request) error

	// framePool recycles frame buffers for track distributors; sized from Config.FrameCapacity in init() (falling back to
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

	// sessionEnds counts checked sessions whose end report hasn't been sent
	// yet (waitSessionEnds).
	sessionEnds pendingCount

	// connectedMu guards connected, which tracks peer addresses already dialing
	// or connected to prevent duplicate maintainPeer goroutines.
	connectedMu sync.Mutex
	connected   map[string]struct{}
	// peerSessions holds the dialed addresses this relay has a session to
	// right now, a subset of connected; also guarded by connectedMu.
	peerSessions map[string]struct{}

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

// HandleWebTransport admits a WebTransport upgrade before it happens: a
// refused client gets the HTTP status (401 or 403, or 503 when it can't be
// checked), and an admitted one carries its grant into the session through
// the request context. Only an upgrade (an extended CONNECT) is checked;
// any other request falls through to the
// WebTransport handler, which answers it without a session.
func (s *Server) HandleWebTransport(w http.ResponseWriter, r *http.Request) {
	s.init()
	if s.webtransportHandler == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	// A request gomoqt won't upgrade goes straight to it, unasked: not an
	// extended CONNECT, or an Origin it refuses. Checking it first would
	// count a session that never starts.
	if r.Method != http.MethodConnect || !s.webtransportHandler.CheckOrigin(r) {
		s.webtransportHandler.ServeHTTP(w, r)
		return
	}
	req := s.webTransportRequest(r)
	g, err := s.admit(r.Context(), req)
	if err != nil {
		w.WriteHeader(auth.RefusalStatus(err))
		return
	}
	a := decidedAdmission(g, req)
	s.webtransportHandler.ServeHTTP(w, r.WithContext(withAdmission(r.Context(), a)))
	// gomoqt serves the session within ServeHTTP. If the upgrade failed
	// anyway, no session ran and none will report its end: report it here,
	// so the session counted at connect is closed.
	if g != nil && !a.served.Load() {
		s.sessionEnds.add()
		s.reportEnd(r.Context(), req, moqt.SessionStats{}, endUpgradeFailed, 0)
		s.sessionEnds.done()
	}
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
		// Native QUIC sessions (ALPN "moqt") are relay peers when trusted, and
		// admitted like clients otherwise. WebTransport sessions (ALPN "h3") are
		// clients, admitted at the upgrade (HandleWebTransport).
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
		// handshake and gives its session a pending admission. For native
		// QUIC connections the underlying type satisfies connStatsProvider,
		// so we launch a polling goroutine to collect connection-level stats
		// (RTT, packet loss). WebTransport connections do not satisfy the
		// interface and are silently skipped.
		s.MOQServer.ConnContext = func(ctx context.Context, conn moqt.StreamConn) context.Context {
			if provider, ok := conn.(connStatsProvider); ok {
				addr := conn.RemoteAddr().String()
				s.sampler.addConn(addr, provider)
				sampleConnStats(provider, addr) // immediate first sample
				context.AfterFunc(conn.Context(), func() { s.sampler.removeConn(addr) })
			}
			// relayPeer decides the admission once the session's SETUP has
			// named its path; a subscription arriving before then waits.
			return withAdmission(ctx, pendingAdmission())
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

// waitSessionEnds waits, at most d, for the end reports of sessions that
// have closed but not yet reported: a session reports its end after its
// connection closes, which can be after Shutdown returns. The relay command
// calls it before the verifier's last usage send, so those ends are in it.
func (s *Server) waitSessionEnds(d time.Duration) {
	if !s.sessionEnds.wait(d) {
		slog.Warn("relay: some sessions hadn't reported their end at shutdown", "waited", d)
	}
}

// pendingCount counts work in flight and lets a caller wait for it to reach
// zero. Unlike a sync.WaitGroup, add may race with wait: a session that
// starts while shutdown waits is simply counted.
type pendingCount struct {
	mu   sync.Mutex
	n    int
	zero chan struct{} // closed when n returns to zero
}

func (c *pendingCount) add() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == 0 {
		c.zero = make(chan struct{})
	}
	c.n++
}

func (c *pendingCount) done() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n--
	if c.n == 0 {
		close(c.zero)
	}
}

// wait reports whether the count reached zero within d.
func (c *pendingCount) wait(d time.Duration) bool {
	c.mu.Lock()
	if c.n == 0 {
		c.mu.Unlock()
		return true
	}
	zero := c.zero
	c.mu.Unlock()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-zero:
		return true
	case <-timer.C:
		return false
	}
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

	// Each configured peer host is resolved to all its A/AAAA records (e.g. a
	// Consul DNS name for a group of hubs), and every address is dialed, so an
	// edge acts as a multi-homed L7 load balancer across them.
	for _, peer := range s.Config.Peers {
		for _, addr := range resolvePeerAddrs(ctx, peer.Address) {
			dialPeer(addr)
		}
	}

	wg.Wait()
}

// resolvePeerAddrs resolves one peer entry (host:port) to ip:port for every
// A/AAAA record of its host. An IP address, an unparseable entry, or a host
// that fails to resolve is returned as is, so the dial loop retries it.
func resolvePeerAddrs(ctx context.Context, entry string) []string {
	host, port, err := net.SplitHostPort(entry)
	if err != nil || net.ParseIP(host) != nil {
		return []string{entry}
	}

	lookupCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	ips, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	cancel()
	if err != nil || len(ips) == 0 {
		return []string{entry}
	}

	addrs := make([]string, 0, len(ips))
	for _, ip := range ips {
		addrs = append(addrs, net.JoinHostPort(ip.IP.String(), port))
	}
	return addrs
}

// ConnectedPeers returns the addresses of the peers this relay has dialed
// and holds a session to, for tests. A dial that reached this relay itself,
// or that is between retries, is not among them.
func (s *Server) ConnectedPeers() []string {
	s.connectedMu.Lock()
	defer s.connectedMu.Unlock()
	return slices.Sorted(maps.Keys(s.peerSessions))
}

// setPeerSession records whether this relay holds a session to the peer it
// dialed at addr.
func (s *Server) setPeerSession(addr string, held bool) {
	s.connectedMu.Lock()
	defer s.connectedMu.Unlock()
	if !held {
		delete(s.peerSessions, addr)
		return
	}
	if s.peerSessions == nil {
		s.peerSessions = make(map[string]struct{})
	}
	s.peerSessions[addr] = struct{}{}
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

// peerURL returns the native-QUIC ("moqt") URL for a peer given as host:port.
// Peers dial through Dialer.Dial, the only entry point gomoqt keeps after
// deprecating Dialer.DialQUIC. Resolved addresses come from net.JoinHostPort,
// so an IPv6 literal is already bracketed, as url.Parse requires.
func peerURL(addr string) string {
	return "moqt://" + addr
}

func (s *Server) maintainPeer(ctx context.Context, peer Peer) {
	var backoff = DialBackoff{Base: 1 * time.Second, Max: 30 * time.Second}

	for {
		if ctx.Err() != nil {
			return
		}

		sess, err := s.MOQDialer.Dial(ctx, peerURL(peer.Address), s.TrackMux)
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
			// A peer this relay dialed from its own configuration is trusted:
			// its session carries no admission, so nothing on it is checked.
			// The handshake verified the peer's certificate against the CA
			// for the peering name; the one peer it must not be is this
			// relay.
			if isSelf(sess.ConnectionState().TLS, s.PeerCertificate) {
				// Expected for a name among the peers that resolves to this
				// relay too. A relay sharing this one's certificate looks the
				// same, and would never be peered with: hence the reminder.
				slog.Info("relay: dialed peer is this relay; not dialing it again (a relay sharing this peer certificate looks the same: each needs its own)",
					"address", peer.Address)
				_ = sess.CloseWithError(moqt.NoError, "self")
				return
			}
			_, identity := classify(sess.ConnectionState().TLS)
			slog.Info("relay: peer connected", "address", peer.Address, "identity", identity)
			s.setPeerSession(peer.Address, true)
			s.serveSession(sess)

			<-sess.Context().Done()

			s.setPeerSession(peer.Address, false)
			slog.Info("peer disconnected", "address", peer.Address)

			// Attempt one immediate reconnect with a small random jitter to
			// spread out synchronized reconnects. The jitter prevents a
			// thundering herd when many peers disconnect simultaneously.
			if !jitterDelay(ctx, 100*time.Millisecond) {
				return
			}
			sess, err = s.MOQDialer.Dial(ctx, peerURL(peer.Address), s.TrackMux)
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

// Relay handles inbound WebTransport sessions (publishers and browser
// clients), already admitted at the upgrade (HandleWebTransport).
func (s *Server) Relay(sess *moqt.Session) {
	if admissionFrom(sess.Context()) == nil {
		// Unreachable through HandleWebTransport; refuse rather than run open.
		_ = sess.CloseWithError(moqt.UnauthorizedSessionErrorCode, "not admitted")
		return
	}
	s.serveSession(sess)
}

// relayPeer handles inbound native QUIC sessions. What the handshake made
// the session decides (peer_trust.go): a relay peer is served without
// asking; an internal client may subscribe and announce nothing; any other
// native-QUIC session is admitted exactly like a WebTransport client, since
// speaking the native protocol is not itself proof of being a peer.
func (s *Server) relayPeer(sess *moqt.Session) {
	a := admissionFrom(sess.Context())
	if a == nil {
		// Unreachable through ConnContext; refuse rather than run open.
		_ = sess.CloseWithError(moqt.UnauthorizedSessionErrorCode, "not admitted")
		return
	}
	state := sess.ConnectionState().TLS
	if isSelf(state, s.PeerCertificate) {
		// This relay dialed itself through a name among its peers; the
		// dialing side drops the session too.
		a.decide(refusedGrant, auth.Request{})
		_ = sess.CloseWithError(moqt.NoError, "self")
		return
	}
	switch class, identity := classify(state); class {
	case classPeer:
		slog.Info("relay: peer session", "remote", sess.RemoteAddr(), "identity", identity)
		a.decide(nil, auth.Request{})
		s.serveSession(sess)
		return
	case classInternal:
		slog.Info("relay: internal client session", "remote", sess.RemoteAddr(), "identity", identity)
		a.decideInternal()
		s.serveSession(sess)
		return
	}
	req := s.nativeRequest(sess)
	g, err := s.admit(sess.Context(), req)
	if err != nil {
		// Closed before the decision: a subscribe the client already sent
		// then fails with the session's error (unauthorized), not with the
		// refused grant's "track does not exist", so the client learns why.
		_ = sess.CloseWithError(moqt.UnauthorizedSessionErrorCode, "refused")
		a.decide(refusedGrant, req)
		return
	}
	a.decide(g, req)
	s.serveSession(sess)
}

// serveSession is the shared core for Relay, relayPeer and maintainPeer. The
// session's announcements are checked against its admitted grant.
func (s *Server) serveSession(sess *moqt.Session) {
	s.init()
	g, err := sessionGrant(sess.Context())
	if err != nil {
		return
	}
	a := admissionFrom(sess.Context())
	// An internal client is restricted by its grant but has no credential:
	// nothing to re-check, and no usage to report.
	checked := a != nil && a.grant != nil && !a.internal
	var l *lease
	if checked {
		a.served.Store(true)
		start := time.Now()
		// Registered first so that it runs last: after the close below,
		// with the session's final byte totals and close cause. Counted in
		// sessionEnds, since it can run after Shutdown has returned.
		s.sessionEnds.add()
		defer func() {
			defer s.sessionEnds.done()
			s.reportEnd(sess.Context(), a.req, sess.Stats(), endReason(l, context.Cause(sess.Context())), time.Since(start))
		}()
	}
	defer sess.CloseWithError(moqt.NoError, moqt.NoError.String())
	if checked {
		if l = startLease(sess.Context(), sess, s.Authorize, a.req, a.deadline, a.grant.Revalidate()); l != nil {
			defer l.stop()
		}
	}

	metricSessionsActive.Inc()
	defer metricSessionsActive.Dec()

	addr := sess.RemoteAddr().String()
	s.sampler.addSession(addr, sess)
	sampleSessionStats(sess, addr) // immediate first sample
	defer s.sampler.removeSession(addr)

	slog.Info("relay: new session", "remote", addr, "checked", checked)

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

		if g != nil && !g.Announces(ann.BroadcastPath()) {
			// MoQ has no per-announcement error response, so the publisher
			// receives no explicit rejection: the ANNOUNCE is simply not
			// mirrored into the TrackMux, and the session's other broadcasts
			// continue.
			metricAnnouncementsRefused.Inc()
			slog.Warn("relay: announcement refused: not covered by the session's grant",
				"broadcast_path", ann.BroadcastPath(),
				"remote", addr)
			continue
		}

		handler := newRelayHandler(ann, sess, s.Config.NodeID,
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
// broadcast path: metric accounting, session-end cancellation, and
// registration in the TrackMux. It also schedules recovery —
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
// mobility mechanism; GOAWAY is observed and surfaced via metrics.
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
