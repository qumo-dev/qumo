package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/cors"
	"github.com/qumo-dev/qumo/internal/envconfig"
	"github.com/qumo-dev/qumo/internal/rtsp"
)

// RunRTSPPull connects to an RTSP source (e.g. an IP camera), pulls the stream
// via DESCRIBE/SETUP/PLAY, and republishes it as MoQT so subscribers can
// consume it. It mirrors RunRTSP's MoQT-serve wiring (TrackMux + WebTransport
// + moqt.Server) but replaces the push-server with a pull-client that feeds the
// same Session, with automatic reconnect on failure.
//
// Configuration:
//
//	args[0]           RTSP source URL (rtsp://[user:pass@]host/path)
//	args[1] (opt)     broadcast path (default /live/camera)
//	RTSP_SERVE_ADDR   MoQT listen address (default :4433)
//	CERT_FILE         TLS certificate (default certs/server.crt)
//	KEY_FILE          TLS key (default certs/server.key)
//	CORS_ALLOWED_ORIGINS  comma-separated WebTransport origins
func RunRTSPPull(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: qumo rtsp <rtsp-url> [broadcast-path]")
	}
	srcURL := args[0]
	path := "/live/camera"
	if len(args) > 1 {
		path = args[1]
	}
	serveAddr := envconfig.String("RTSP_SERVE_ADDR", defaultRTMPServeAddr)
	certFile := envconfig.String("CERT_FILE", "certs/server.crt")
	keyFile := envconfig.String("KEY_FILE", "certs/server.key")
	allowedOrigins := cors.LoadAllowed()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	handle, err := PullAndServe(ctx, PullConfig{
		SourceURL:      srcURL,
		BroadcastPath:  path,
		ServeAddr:      serveAddr,
		CertFile:       certFile,
		KeyFile:        keyFile,
		AllowedOrigins: allowedOrigins,
	})
	if err != nil {
		return err
	}
	handle.Wait()
	handle.Close()
	return nil
}

// PullConfig configures a PullAndServe call.
type PullConfig struct {
	SourceURL      string
	BroadcastPath  string
	ServeAddr      string
	CertFile       string
	KeyFile        string
	AllowedOrigins []string
}

// PullHandle is a running RTSP pull ingest. Close stops the pull + MoQT server;
// Wait blocks until the pull exits (error or context cancel).
type PullHandle struct {
	cancel  context.CancelFunc
	done    chan struct{}
	sess    *Session
	moqSrv  *moqt.Server
	srcURL  string
	path    string
	lastErr atomic.Value // string
}

// Close stops the pull and MoQT server.
func (h *PullHandle) Close() {
	h.cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutCancel()
	_ = h.moqSrv.Shutdown(shutCtx)
	h.sess.Close()
}

// Wait blocks until the pull exits.
func (h *PullHandle) Wait() {
	<-h.done
}

// LastErr returns the last error from the pull loop, or "" if none.
func (h *PullHandle) LastErr() string {
	if v := h.lastErr.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// SourceURL returns the (redacted) source URL.
func (h *PullHandle) SourceURL() string {
	return redactURL(h.srcURL)
}

// Path returns the broadcast path.
func (h *PullHandle) Path() string {
	return h.path
}

// PullAndServe starts an RTSP pull ingest: dials the source, creates a Session
// on a fresh TrackMux, serves MoQT (WebTransport) on serveAddr, and runs the
// pull loop with reconnect. The returned PullHandle lets the caller stop the
// pull (Close) or block until it exits (Wait).
func PullAndServe(parentCtx context.Context, cfg PullConfig) (*PullHandle, error) {
	ctx, cancel := context.WithCancel(parentCtx)

	trackMux := moqt.NewTrackMux(0)
	sess, err := NewSession(trackMux, moqt.BroadcastPath(cfg.BroadcastPath))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("rtsp pull session: %w", err)
	}

	wtHandler := &moqt.WebTransportHandler{
		TrackMux:    trackMux,
		CheckOrigin: cors.NewChecker(cfg.AllowedOrigins),
		Handler: moqt.HandleFunc(func(sess *moqt.Session) {
			defer sess.CloseWithError(moqt.NoError, moqt.NoError.String())
			<-sess.Context().Done()
		}),
	}
	mux := http.NewServeMux()
	mux.Handle("/", wtHandler)
	moqSrv := &moqt.Server{
		Addr:               cfg.ServeAddr,
		WebTransportServer: moqt.NewWebTransportServer(mux),
		TrackMux:           trackMux,
	}
	go func() {
		if err := moqSrv.ListenAndServeTLS(cfg.CertFile, cfg.KeyFile); err != nil && ctx.Err() == nil {
			slog.Error("MoQT server error", "err", err)
			cancel()
		}
	}()

	h := &PullHandle{
		cancel: cancel,
		done:   make(chan struct{}),
		sess:   sess,
		moqSrv: moqSrv,
		srcURL: cfg.SourceURL,
		path:   cfg.BroadcastPath,
	}

	slog.Info("RTSP pull ingest starting",
		"source", redactURL(cfg.SourceURL), "broadcast_path", cfg.BroadcastPath, "serve", cfg.ServeAddr)

	go func() {
		defer close(h.done)
		backoff := 2 * time.Second
		for ctx.Err() == nil {
			err := pullStream(ctx, cfg.SourceURL, sess)
			if ctx.Err() != nil {
				return
			}
			h.lastErr.Store(err.Error())
			slog.Warn("RTSP pull disconnected, reconnecting", "error", err, "backoff", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}()

	return h, nil
}

// pullStream connects to the RTSP source, sets up all tracks, and reads
// interleaved RTP frames until an error occurs (connection drop, PLAY failure,
// or context cancellation). The caller handles reconnection.
func pullStream(ctx context.Context, srcURL string, sess *Session) error {
	client, err := rtsp.Dial(ctx, srcURL)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer client.Close()

	sdp, err := client.Describe(ctx)
	if err != nil {
		return fmt.Errorf("describe: %w", err)
	}

	// SETUP each supported media track on its own interleaved channel pair.
	channelToTrack := make(map[uint8]*rtspTrack)
	nextChan := 0
	for i := range sdp.Medias {
		media := &sdp.Medias[i]
		track := newRTSPTrackFromMedia(sess, media)
		// Skip tracks where codec detection didn't match. trackKindVideo is the
		// zero value, so kind alone can't tell us — check the codec-specific
		// fields (avcCfg for H.264, aacDepack for AAC).
		isVideo := track.kind == trackKindVideo && track.avcCfg != nil
		isAudio := track.kind == trackKindAudio && track.aacDepack != nil
		if !isVideo && !isAudio {
			slog.Info("skipping unsupported RTSP media",
				"type", media.Type, "rtpmap", media.RtpMap)
			continue
		}
		trackURL := resolveControlURL(media.Control, srcURL)
		// Security: reject SDP control attributes that point to a different
		// origin (SSRF via malicious SDP). The resolved track URL must share
		// the session URL's scheme + host + port.
		if !sameOrigin(trackURL, srcURL) {
			slog.Warn("RTSP track control URL points to a different origin, skipping (possible SSRF)",
				"track_control", media.Control)
			continue
		}
		rtpCh := uint8(nextChan)
		rtcpCh := uint8(nextChan + 1)
		nextChan += 2
		setup, err := client.Setup(ctx, trackURL, rtpCh, rtcpCh)
		if err != nil {
			return fmt.Errorf("setup %s: %w", trackURL, err)
		}
		channelToTrack[setup.RTP] = track
		slog.Info("RTSP track set up",
			"type", media.Type, "rtpmap", media.RtpMap,
			"rtp_channel", setup.RTP, "rtcp_channel", setup.RTCP)
	}

	if len(channelToTrack) == 0 {
		return fmt.Errorf("no supported media tracks (need H.264 video and/or mpeg4-generic audio)")
	}

	if err := client.Play(ctx); err != nil {
		return fmt.Errorf("play: %w", err)
	}
	slog.Info("RTSP pull streaming started", "source", srcURL, "tracks", len(channelToTrack))

	// Start a keepalive goroutine to prevent the server from dropping the
	// session. RTSP servers (e.g. mediamtx) tear down sessions after the
	// timeout window (typically 60s) if no keepalive is received.
	stopKeepalive := startKeepalive(ctx, client)
	defer stopKeepalive()

	for ctx.Err() == nil {
		frame, err := client.ReadInterleaved()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		track, ok := channelToTrack[frame.Channel]
		if !ok {
			continue // RTCP or unknown channel → discard.
		}
		track.handleFrame(frame)
	}
	return nil
}

// defaultSessionTimeout is assumed when the RTSP server does not advertise a
// timeout in the Session header (RFC 2326 §12.37 defaults to 60 seconds).
const defaultSessionTimeout = 60 * time.Second

// startKeepalive launches a background goroutine that sends periodic
// GET_PARAMETER requests to keep the RTSP session alive. It returns a stop
// function that cancels the goroutine. The keepalive interval is half the
// session timeout, clamped to [5s, 30s].
func startKeepalive(ctx context.Context, client *rtsp.Client) func() {
	timeout := client.SessionTimeout()
	if timeout == 0 {
		timeout = defaultSessionTimeout
	}
	interval := timeout / 2
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := client.SendKeepalive(); err != nil {
					slog.Debug("RTSP keepalive failed", "error", err)
					return
				}
				slog.Debug("RTSP keepalive sent", "interval", interval)
			case <-ctx.Done():
				return
			case <-done:
				return
			}
		}
	}()

	return func() { close(done) }
}

// resolveControlURL resolves an SDP media's a=control URL against the session
// (DESCRIBE) URL. Handles absolute URLs (returned as-is), relative URLs
// (resolved against the session URL as a "directory"), and "*" or empty
// (returns the session URL).
func resolveControlURL(control, sessionURL string) string {
	if control == "" || control == "*" {
		return sessionURL
	}
	if strings.Contains(control, "://") {
		return control // absolute
	}
	base, err := url.Parse(sessionURL)
	if err != nil {
		return sessionURL
	}
	// Append "/" so ResolveReference treats the session path as a directory
	// (appends the relative control) rather than replacing the last segment.
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}
	ref, err := url.Parse(control)
	if err != nil {
		return sessionURL
	}
	return base.ResolveReference(ref).String()
}

// redactURL strips credentials (user:pass@) from a URL string so it is safe to
// log without leaking passwords.
func redactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL // best-effort: return as-is if unparseable
	}
	u.User = nil
	return u.String()
}

// sameOrigin reports whether two URLs share the same scheme, host, and port.
// Used to reject SDP a=control attributes that redirect to a different server
// (SSRF prevention).
func sameOrigin(a, b string) bool {
	ua, err := url.Parse(a)
	if err != nil {
		return false
	}
	ub, err := url.Parse(b)
	if err != nil {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && ua.Host == ub.Host
}
