package hls

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/gomoqt/msf"

	"github.com/okdaichi/qumo-ledger/ledger"
	"github.com/okdaichi/qumo-ledger/ledger/store"

	"github.com/qumo-dev/qumo/internal/cmaf"
	"github.com/qumo-dev/qumo/internal/relay"
	"github.com/qumo-dev/qumo/internal/relaycred"
	"github.com/qumo-dev/qumo/internal/tlsclient"
	"github.com/qumo-dev/qumo/token"
)

// announcePrefix is where publishers opt into the egress: announcing
// /.hls/live/cam1 asks it to serve the broadcast /live/cam1.
const announcePrefix = token.HLSDirectory

// feedConfig carries the relay subscription parameters, resolved from
// environment by [Run].
type feedConfig struct {
	relayURL string // relay URL, e.g. "https://host:4433"
	// caFile, when set, is a PEM cert trusted as the relay's root, overriding
	// the system roots. Empty means verify against the system root store.
	caFile string
	// insecure skips relay TLS verification entirely for a self-signed dev
	// relay. It dominates caFile when both are set — matching crypto/tls, where
	// InsecureSkipVerify short-circuits verification regardless of RootCAs.
	insecure bool
	// certFile and keyFile, when set, are a peer certificate and key (PEM)
	// for the egress, from the relay CA the relay trusts as CA_FILE. The
	// relay then serves the egress as a relay peer, which needs no
	// credential and has no grant to expire. Only a native-QUIC (moqt://)
	// relayURL presents it.
	certFile, keyFile string
	// signingKey, when set, signs the egress's credential: a fresh one on
	// every connect, in place of any ?jwt= in relayURL, granting subscribe
	// on the whole namespace, and a fresh one on the session before its grant
	// lapses. A token lives an hour at most, and its grant with it, so one
	// fixed in relayURL lets the egress subscribe for that hour alone.
	signingKey *token.SigningKey
	// quic is the QUIC configuration of the session, whether moqt:// or
	// https://; nil is quic-go's defaults.
	quic *quic.Config

	// liveTimeout is how long the feed waits for a group before deciding the
	// publisher is gone. It is also how stale a manifest may be before the
	// server stops claiming the stream is live — one silence, described once.
	liveTimeout time.Duration
}

// mediaInfo is the selected media track's identity and ledger projection,
// learned from the MSF catalog: schema drives the ledger track, and init is the
// fMP4 initialization segment carried in the catalog's initData.
type mediaInfo struct {
	path   moqt.BroadcastPath
	name   moqt.TrackName
	schema ledger.TrackSchema

	// packager turns the stored groups of LOC frames into the CMAF this egress
	// serves. It also owns the init segment, which it derives from the catalog
	// rather than receiving — so it exists before any media does, and the feed
	// never waits on a publisher to describe itself twice.
	packager *cmaf.Packager

	role    string
	codec   string
	width   uint16
	height  uint16
	bitrate int64
}

// connectRetryInterval is how long the egress waits between attempts to reach
// the relay, and before resubscribing a track whose feed ended.
const connectRetryInterval = 2 * time.Second

// validate checks the settings that would otherwise fail only at the first
// dial, or silently: a client certificate the relay would never see or take
// for a peer's, a credential beside a certificate that makes the relay
// ignore it, a signing key that can't grant the egress the namespace.
func (c feedConfig) validate() error {
	u, err := url.Parse(c.relayURL)
	if err != nil {
		// url.Parse's *url.Error quotes the whole URL, and its query may
		// carry a credential; report only the reason it wraps.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return fmt.Errorf("RELAY_URL: invalid URL: %w", err)
	}
	if c.certFile == "" && c.keyFile == "" {
		if c.signingKey == nil {
			return nil
		}
		// The egress subscribes beneath announcePrefix and at the broadcast
		// each announcement names with that prefix stripped: no root short
		// of "/" holds both.
		if p := c.signingKey.Prefix; p != "" && p != "/" {
			return fmt.Errorf("RELAY_SIGNING_KEY is confined to %q, but the egress reads announcements under %q and the broadcasts they name: use a key for the whole namespace (prefix \"/\")",
				p, announcePrefix)
		}
		// Signing one now reports an unusable key at startup rather than
		// at the first dial.
		if _, err := token.Sign(*c.signingKey, c.grant(), token.MaxLifetime); err != nil {
			return fmt.Errorf("RELAY_SIGNING_KEY: %w", err)
		}
		return nil
	}
	if c.certFile == "" || c.keyFile == "" {
		return errors.New("RELAY_CERT_FILE and RELAY_KEY_FILE must be set together")
	}
	if u.Scheme != "moqt" {
		// A WebTransport session presents no certificate: the relay checks
		// the credential of every one.
		return fmt.Errorf("RELAY_CERT_FILE needs a native-QUIC RELAY_URL (moqt://), got %q", u.Scheme)
	}
	// The relay serves a session with a peer certificate as a peer and
	// never reads its credential: a credential beside one limits nothing.
	if c.signingKey != nil {
		return errors.New("RELAY_SIGNING_KEY and RELAY_CERT_FILE are two ways to connect: set one")
	}
	if u.Query().Has("jwt") {
		return errors.New("RELAY_CERT_FILE makes the egress a relay peer, whose credential the relay never reads: remove ?jwt= from RELAY_URL, or the certificate")
	}
	return checkPeerCertificate(c.certFile, c.keyFile)
}

// checkPeerCertificate reports why the relay would not take the certificate
// in certFile for a peer's: it doesn't load, or doesn't carry the peering
// name. Whether the relay's CA issued it is the relay's to say.
func checkPeerCertificate(certFile, keyFile string) error {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("RELAY_CERT_FILE: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return fmt.Errorf("RELAY_CERT_FILE: %w", err)
	}
	if !slices.Contains(leaf.DNSNames, relay.PeeringName) {
		return fmt.Errorf("RELAY_CERT_FILE: the certificate doesn't carry the peering name %q among its DNS names, so the relay would ask the egress for a credential: use a peer certificate, or RELAY_SIGNING_KEY in its place", relay.PeeringName)
	}
	return nil
}

// grant returns the token grant the egress's session needs: subscribing
// anywhere. The announcements under announcePrefix and the broadcasts they name
// both have to lie within the root, so it is the whole namespace; validate
// refuses a key confined to less.
func (c feedConfig) grant() token.Grant {
	return token.Grant{Root: "/", Subscribe: []string{"/"}}
}

// target returns the URL to dial, with a freshly signed credential when the
// egress signs its own, and when that credential's grant lapses.
func (c feedConfig) target() (string, time.Time, error) {
	if c.signingKey == nil {
		return c.relayURL, time.Time{}, nil
	}
	credential, lapses, err := relaycred.Sign(*c.signingKey, c.grant())
	if err != nil {
		return "", time.Time{}, err
	}
	u, err := url.Parse(c.relayURL)
	if err != nil {
		return "", time.Time{}, errors.New("RELAY_URL: invalid URL")
	}
	query := u.Query()
	query.Set("jwt", credential)
	u.RawQuery = query.Encode()
	return u.String(), lapses, nil
}

// relayTLSConfig builds the client TLS config for dialing the relay. The egress
// verifies the relay's certificate by default: against the system root store
// when caFile is empty, or against a single relay cert when caFile names a PEM.
// insecure opts out of verification entirely for a self-signed dev relay. The
// trust decision itself is shared across qumo's clients via [tlsclient.Apply];
// only the egress-specific base config (TLS 1.3 floor) is owned here.
func relayTLSConfig(caFile string, insecure bool) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS13}
	if err := tlsclient.Apply(tc, caFile, insecure); err != nil {
		return nil, err
	}
	return tc, nil
}

// dialRelay dials the relay session and registers credential renewal when
// signingKey is set.
func dialRelay(ctx context.Context, cfg feedConfig) (*moqt.Session, error) {
	tc, err := relayTLSConfig(cfg.caFile, cfg.insecure)
	if err != nil {
		return nil, fmt.Errorf("hls: relay TLS config: %w", err)
	}
	if err := tlsclient.ApplyClientCert(tc, cfg.certFile, cfg.keyFile); err != nil {
		return nil, fmt.Errorf("hls: relay client certificate: %w", err)
	}
	target, lapses, err := cfg.target()
	if err != nil {
		return nil, fmt.Errorf("hls: %w", err)
	}
	// The egress follows redirects itself (subscribeFollowing), so that it can
	// tell the opt-in's answer from the broadcast's.
	dialer := &moqt.Dialer{TLSConfig: tc, QUICConfig: cfg.quic, CheckRedirect: relay.StopRedirect}
	session, err := dialer.Dial(ctx, target, moqt.NewTrackMux(0))
	if err != nil {
		return nil, fmt.Errorf("hls: dial relay: %w", err)
	}
	if cfg.signingKey != nil {
		go relaycred.Keep(session, *cfg.signingKey, cfg.grant(), lapses)
	}
	return session, nil
}

// dialRelayWithRetry calls dialRelay repeatedly until it succeeds or ctx is
// cancelled.
func dialRelayWithRetry(ctx context.Context, cfg feedConfig) (*moqt.Session, error) {
	for attempt := 1; ; attempt++ {
		session, err := dialRelay(ctx, cfg)
		if err == nil {
			return session, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		slog.Warn("hls: waiting for relay, retrying", "attempt", attempt, "url", cfg.relayURL, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(connectRetryInterval):
		}
	}
}

// newMediaInfo describes catalog track t of the broadcast at path, packaged by
// packager.
func newMediaInfo(path string, t *msf.Track, packager *cmaf.Packager) mediaInfo {
	var w, h uint16
	if t.Width != nil {
		w = uint16(*t.Width)
	}
	if t.Height != nil {
		h = uint16(*t.Height)
	}
	var br int64
	if t.Bitrate != nil {
		br = *t.Bitrate
	} else if t.AvgBitrate != nil {
		br = *t.AvgBitrate
	}
	role := string(t.Role)
	if role == "" {
		role = "video"
	}
	return mediaInfo{
		path: moqt.BroadcastPath(path),
		name: moqt.TrackName(t.Name),
		schema: ledger.TrackSchema{
			Timescale:  cmaf.Timescale,
			TimeSource: ledger.TimeSourceIngest,
			MIME:       t.MimeType,
			Encoding:   encodingMoQLite05,
		},
		packager: packager,
		role:     role,
		codec:    t.Codec,
		width:    w,
		height:   h,
		bitrate:  br,
	}
}

// fetchCatalogWithRetry subscribes to the catalog track with short retries to
// accommodate any startup race between announcement arrival and the publisher's
// catalog publication. See optInCatalog for optIn, broadcast and the path it
// returns. An answer a retry cannot change ends the retries.
func fetchCatalogWithRetry(ctx context.Context, session *moqt.Session, optIn, broadcast string) (msf.Catalog, moqt.BroadcastPath, error) {
	c := &optInCatalog{optIn: optIn, broadcast: broadcast}
	for attempt := 1; attempt <= 10; attempt++ {
		cat, source, err := c.fetch(ctx, session)
		if err == nil || errors.Is(err, errNotRetryable) {
			return cat, source, err
		}
		if ctx.Err() != nil {
			return msf.Catalog{}, "", ctx.Err()
		}
		select {
		case <-ctx.Done():
			return msf.Catalog{}, "", ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return c.fetch(ctx, session)
}

// discoverMediaTracks finds all packageable media tracks described in the MSF
// catalog. Everything needed to describe a track is stated in the catalog, so
// its packager — and with it the init segment — is built here, before a single
// frame arrives.
func discoverMediaTracks(c msf.Catalog, broadcastPath string) []mediaInfo {
	var infos []mediaInfo
	for i := range c.Tracks {
		t := &c.Tracks[i]
		packager, err := packagerForTrack(c, t)
		if err != nil {
			continue
		}
		infos = append(infos, newMediaInfo(broadcastPath, t, packager))
	}
	return infos
}

// broadcastGate lets one announcement handler at a time feed a broadcast. A
// relay that reconnects or re-announces starts a new handler while the old one
// may still be closing its ledger epochs; the new one waits, so two writers
// never share a ledger track.
type broadcastGate struct {
	mu   sync.Mutex
	held map[string]chan struct{} // closed when the holder releases
}

func newBroadcastGate() *broadcastGate {
	return &broadcastGate{held: make(map[string]chan struct{})}
}

// acquire waits until no other handler holds broadcast, then holds it. It
// returns the release function, or ctx's error if ctx ends first.
func (g *broadcastGate) acquire(ctx context.Context, broadcast string) (func(), error) {
	for {
		g.mu.Lock()
		held, busy := g.held[broadcast]
		if !busy {
			done := make(chan struct{})
			g.held[broadcast] = done
			g.mu.Unlock()
			return func() {
				g.mu.Lock()
				defer g.mu.Unlock()
				delete(g.held, broadcast)
				close(done)
			}, nil
		}
		g.mu.Unlock()

		select {
		case <-held:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// handleAnnouncement processes an announcement for a broadcast: it strips the
// announcement prefix to derive the real broadcast path, fetches its catalog,
// discovers all packageable media tracks, registers them with mux, and feeds
// them into ledger tracks until the announcement ends.
func handleAnnouncement(ctx context.Context, session *moqt.Session, ann *moqt.Announcement, store store.Store, mux *streamMux, gate *broadcastGate, window uint64, cfg feedConfig) {
	annPath := string(ann.BroadcastPath())
	realBroadcast := broadcastPathFromAnnounce(annPath, announcePrefix)
	slog.Info("hls: received announcement", "announce", annPath, "broadcast", realBroadcast)

	annCtx, cancelAnn := context.WithCancel(ctx)
	defer cancelAnn()

	go func() {
		select {
		case <-ann.Done():
			cancelAnn()
		case <-annCtx.Done():
		}
	}()

	catalog, source, err := fetchCatalogWithRetry(annCtx, session, annPath, realBroadcast)
	if err != nil {
		slog.Warn("hls: catalog fetch failed for announced broadcast", "broadcast", realBroadcast, "err", err)
		return
	}
	if string(source) != realBroadcast {
		slog.Info("hls: opt-in redirected", "announce", annPath, "source", source)
	}

	// The media tracks are subscribed where the catalog came from; the
	// broadcast keeps its name in the egress's URLs and storage.
	mediaTracks := discoverMediaTracks(catalog, string(source))
	if len(mediaTracks) == 0 {
		slog.Warn("hls: no packageable media tracks found", "broadcast", realBroadcast)
		return
	}

	// Wait for an earlier handler of this broadcast to close its epochs before
	// opening writers on the same ledger tracks.
	release, err := gate.acquire(annCtx, realBroadcast)
	if err != nil {
		return
	}
	defer release()

	bEntry := mux.RegisterBroadcast(realBroadcast)
	defer mux.UnregisterBroadcast(bEntry)

	type activeTrack struct {
		m      mediaInfo
		writer *ledger.Writer
		live   *liveness
	}
	var active []activeTrack

	cleanBroadcast := strings.Trim(realBroadcast, "/")
	for _, m := range mediaTracks {
		ledgerTrackPath := ledger.TrackPath(cleanBroadcast + "/" + string(m.name))
		track, adopted, err := openTrack(annCtx, store, ledgerTrackPath, m.schema)
		if err != nil {
			slog.Error("hls: open track error", "path", ledgerTrackPath, "err", err)
			continue
		}
		writer, err := track.Writer(annCtx)
		if err != nil {
			slog.Error("hls: open writer error", "path", ledgerTrackPath, "err", err)
			continue
		}
		if adopted {
			if err := writer.NewEpoch(annCtx); err != nil {
				slog.Error("hls: new epoch error", "path", ledgerTrackPath, "err", err)
				continue
			}
		}

		live := &liveness{}
		handler, err := newStreamHandler(track, streamOptions{
			Packager:    m.packager,
			Window:      int(window),
			EpochWindow: 1,
			QueryParam:  mux.access.param,
		})
		if err != nil {
			slog.Error("hls: build stream handler error", "path", ledgerTrackPath, "err", err)
			continue
		}

		trackHandler := withLiveness(handler, live, cfg.liveTimeout)
		bEntry.AddTrack(trackEntry{
			info: trackInfo{
				Name:    string(m.name),
				Role:    m.role,
				Codec:   m.codec,
				Width:   m.width,
				Height:  m.height,
				Bitrate: m.bitrate,
			},
			handler: trackHandler,
			live:    live,
		})

		active = append(active, activeTrack{
			m:      m,
			writer: writer,
			live:   live,
		})
	}

	if len(active) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, at := range active {
		wg.Add(1)
		go func(at activeTrack) {
			defer wg.Done()
			feedAnnouncedTrack(annCtx, sessionSubscriber{session}, bEntry, at.m, at.writer, at.live, cfg)
		}(at)
	}

	<-annCtx.Done()
	wg.Wait()
	slog.Info("hls: announcement finished", "broadcast", realBroadcast)
}

// epochCloseTimeout bounds closing a finished feed's epoch once its
// announcement has ended, when no other context limits the ledger write.
const epochCloseTimeout = 10 * time.Second

// feedAnnouncedTrack feeds one track of an announced broadcast for as long as
// the announcement (ctx) lasts. A feed that ends while the broadcast is still
// announced — a silent publisher, a refused subscribe — subscribes again,
// rather than leaving the track listed in the master playlist with nothing
// behind it. A feed that committed groups closes its epoch first: that empties
// a manifest scoped to the current lifetime, and gives the next feed its own
// sequence space, since a publisher numbers groups from zero and appending
// those to a filled epoch collides with IDs already committed. A feed that
// committed nothing leaves the epoch open, so a track that keeps failing to
// subscribe does not fill the ledger with empty epochs. On return an epoch
// still holding groups is closed, so the manifest stops listing a finished
// session.
func feedAnnouncedTrack(ctx context.Context, sub mediaSubscriber, b *broadcastEntry, m mediaInfo, w epochAppender, live *liveness, cfg feedConfig) {
	// filled records that the current epoch holds groups not yet closed off.
	var filled bool
	defer func() {
		if !filled {
			return
		}
		closeCtx, cancel := context.WithTimeout(context.Background(), epochCloseTimeout)
		defer cancel()
		if err := w.NewEpoch(closeCtx); err != nil {
			slog.Warn("hls: cannot close the finished track's epoch", "broadcast", m.path, "track", m.name, "err", err)
		}
	}()
	for {
		fed := &countingAppender{groupAppender: w}
		if err := feedMedia(ctx, sub, m, fed, live, cfg); err != nil && ctx.Err() == nil {
			slog.Warn("hls: track feed ended", "broadcast", m.path, "track", m.name, "err", err)
		}
		filled = filled || fed.appended > 0
		if ctx.Err() != nil {
			return
		}
		if filled {
			if err := w.NewEpoch(ctx); err != nil {
				// The ledger can't take a restarted feed's groups: unlist the
				// track rather than serve its stale segments.
				slog.Error("hls: cannot close the track's epoch; unlisting it", "broadcast", m.path, "track", m.name, "err", err)
				b.RemoveTrack(string(m.name))
				return
			}
			filled = false
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(connectRetryInterval):
		}
	}
}

// runAnnounceEgress dials the relay and listens for announcements beneath
// announcePrefix. Each announced broadcast is dynamically packaged and served
// over HTTP until the announcement closes or the session ends. It returns once
// ctx is done and every announcement it started has finished.
func runAnnounceEgress(ctx context.Context, store store.Store, mux *streamMux, window uint64, cfg feedConfig) {
	gate := newBroadcastGate()
	var handlers sync.WaitGroup
	defer handlers.Wait()
	for {
		session, err := dialRelayWithRetry(ctx, cfg)
		if err != nil {
			return
		}

		slog.Info("hls: connected to relay, accepting announcements", "prefix", announcePrefix)
		announced, err := session.AcceptAnnounce(announcePrefix)
		if err != nil {
			slog.Warn("hls: accept announce failed", "err", sessionRefusal(session, err))
			// not actionable: the session is being abandoned either way.
			_ = session.CloseWithError(moqt.NoError, "accept announce failed")
		} else {
			// Ends with the session or with ctx, so shutting down reaches the
			// receive loop and every announcement's feeds.
			sessCtx, stop := context.WithCancel(ctx)
			stopWithSession := context.AfterFunc(session.Context(), stop)
			for {
				ann, err := announced.ReceiveAnnouncement(sessCtx)
				if err != nil {
					if ctx.Err() == nil {
						slog.Warn("hls: announce stream ended", "err", err)
					}
					break
				}
				handlers.Go(func() {
					handleAnnouncement(sessCtx, session, ann, store, mux, gate, window, cfg)
				})
			}
			stopWithSession()
			stop()
			// not actionable: the session is being abandoned either way.
			_ = session.CloseWithError(moqt.NoError, "reconnecting")
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(connectRetryInterval):
		}
	}
}

// packagerForTrack builds the CMAF packager a catalog track describes.
func packagerForTrack(c msf.Catalog, t *msf.Track) (*cmaf.Packager, error) {
	if t.Width == nil || t.Height == nil {
		return nil, fmt.Errorf("hls: track %q states no picture size", t.Name)
	}
	return cmaf.NewPackager(cmaf.VideoConfig{
		Codec:  t.Codec,
		Width:  uint16(*t.Width),
		Height: uint16(*t.Height),
		// AVC and HEVC carry their parameter sets out of band; the LOC
		// publisher puts them in the catalog's InitDataList, referenced by
		// the track's initRef.
		Description: initFromTrack(c, t),
	})
}

// sessionRefusal returns what to report for a subscribe that failed with
// err. A relay that won't serve a session closes it with "unauthorized"
// before answering anything, so a subscribe in flight fails with that
// close; the session's cause then says so, and the two ways in are named.
// Any other close by the relay is reported as such; otherwise err.
func sessionRefusal(session *moqt.Session, err error) error {
	// moqt.Cause gives the close as a SessionError on either transport.
	cause := moqt.Cause(session.Context())
	sessErr, ok := errors.AsType[*moqt.SessionError](cause)
	if !ok || !sessErr.Remote {
		return err
	}
	if sessErr.SessionErrorCode() == moqt.UnauthorizedSessionErrorCode {
		return fmt.Errorf("hls: the relay refused the session (%w): connect with a signing key from its key set (RELAY_SIGNING_KEY), a credential it still accepts (?jwt= in RELAY_URL), or a peer certificate from its CA (RELAY_CERT_FILE)", cause)
	}
	return fmt.Errorf("hls: the relay closed the session: %w", cause)
}

// maxRedirectHops bounds the redirects the egress follows for one
// subscription, as gomoqt's Session.Subscribe does by default.
const maxRedirectHops = 2

// errNotRetryable marks an answer a retry cannot change: a redirect to
// another relay, or more redirects than the egress follows.
var errNotRetryable = errors.New("hls: not retryable")

// subscribeFollowing subscribes to name at path on session, whose
// CheckRedirect stops at every redirect (relay.StopRedirect), and follows the
// redirects itself: up to maxRedirectHops, on this session. hops reports how
// many it followed, so a caller can tell an answer of path's own from one of
// a redirect's target.
func subscribeFollowing(ctx context.Context, session *moqt.Session, path moqt.BroadcastPath, name moqt.TrackName) (tr *moqt.TrackReader, hops int, err error) {
	for {
		tr, err := session.Subscribe(ctx, path, name, &moqt.SubscribeConfig{Ordered: true})
		redirectErr, ok := errors.AsType[*moqt.RedirectError](err)
		if !ok {
			return tr, hops, err
		}
		to := redirectErr.Redirect
		if to.URI != "" {
			return nil, hops, fmt.Errorf("%w: %s %s redirected to another relay, which the egress does not dial", errNotRetryable, path, name)
		}
		if hops == maxRedirectHops {
			return nil, hops, fmt.Errorf("%w: %s %s: %w", errNotRetryable, path, name, moqt.ErrTooManyRedirects)
		}
		path, name = to.BroadcastPath, to.TrackName
		hops++
	}
}

// optInCatalog fetches the catalog of a broadcast opted into the egress by the
// announcement optIn. The opt-in redirects to its broadcast, which the egress
// follows. A publisher that predates the redirect answers NOT_FOUND instead,
// and its broadcast is broadcast, the opt-in's path without announcePrefix:
// legacy remembers that, so a retry asks the broadcast directly. NOT_FOUND
// after a redirect is an error, retried: the target is not announced yet.
type optInCatalog struct {
	optIn, broadcast string
	legacy           bool
}

// fetch subscribes to the catalog track and parses its first group as the MSF
// catalog. It returns the path the catalog came from.
func (c *optInCatalog) fetch(ctx context.Context, session *moqt.Session) (msf.Catalog, moqt.BroadcastPath, error) {
	cat, err := c.subscribe(ctx, session)
	if err != nil {
		return msf.Catalog{}, "", err
	}
	defer cat.Close()

	gr, err := cat.AcceptGroup(ctx)
	if err != nil {
		return msf.Catalog{}, "", fmt.Errorf("hls: read catalog group: %w", err)
	}
	data, err := drainRaw(gr, moqt.NewFrame(0))
	if err != nil {
		return msf.Catalog{}, "", fmt.Errorf("hls: read catalog payload: %w", err)
	}

	catalog, err := msf.ParseCatalog(data)
	if err != nil {
		return msf.Catalog{}, "", fmt.Errorf("hls: parse catalog: %w", err)
	}
	return catalog, cat.BroadcastPath, nil
}

// subscribe subscribes to the catalog track as fetch describes.
func (c *optInCatalog) subscribe(ctx context.Context, session *moqt.Session) (*moqt.TrackReader, error) {
	if !c.legacy {
		cat, hops, err := subscribeFollowing(ctx, session, moqt.BroadcastPath(c.optIn), msf.DefaultCatalogTrackName)
		subErr, notFound := errors.AsType[*moqt.SubscribeError](err)
		notFound = notFound && subErr.SubscribeErrorCode() == moqt.SubscribeErrorCodeNotFound
		if hops > 0 || !notFound {
			if err != nil {
				return nil, fmt.Errorf("hls: subscribe catalog through the opt-in %s: %w", c.optIn, err)
			}
			return cat, nil
		}
		c.legacy = true
	}
	cat, _, err := subscribeFollowing(ctx, session, moqt.BroadcastPath(c.broadcast), msf.DefaultCatalogTrackName)
	if err != nil {
		return nil, fmt.Errorf("hls: subscribe catalog at %s, as the opt-in %s answered NOT_FOUND: %w", c.broadcast, c.optIn, err)
	}
	return cat, nil
}

// fetchCatalog fetches the catalog of the broadcast optIn opted into the
// egress, once; see optInCatalog.
func fetchCatalog(ctx context.Context, session *moqt.Session, optIn, broadcast string) (msf.Catalog, moqt.BroadcastPath, error) {
	return (&optInCatalog{optIn: optIn, broadcast: broadcast}).fetch(ctx, session)
}

// OptInHandler serves an announcement beneath announcePrefix that opts
// broadcast into the egress: it redirects every subscription to broadcast,
// keeping the track name, and refuses a subscriber that does not follow
// redirects with NOT_FOUND.
func OptInHandler(broadcast moqt.BroadcastPath) moqt.TrackHandler {
	return moqt.TrackHandlerFunc(func(tw *moqt.TrackWriter) {
		if err := tw.Redirect(tw.Context(), moqt.Redirect{BroadcastPath: broadcast}); err != nil {
			tw.CloseWithError(moqt.SubscribeErrorCodeNotFound)
		}
	})
}

// initFromTrack base64-decodes the fMP4 init data the track's initRef points
// at in the catalog's InitDataList, returning nil when the track carries no
// initRef, the ref is unresolved, or the data is malformed.
func initFromTrack(c msf.Catalog, t *msf.Track) []byte {
	if t.InitRef == "" {
		return nil
	}
	var data string
	for i := range c.InitDataList {
		if c.InitDataList[i].ID == t.InitRef {
			data = c.InitDataList[i].Data
			break
		}
	}
	if data == "" {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		// Treated as absent: the packager rejects a track it cannot describe
		// (an AVC catalog without parameter sets) with a clearer message than a
		// base64 error would give here.
		return nil
	}
	return b
}

// The feed talks to two libraries it cannot stand up in a test — a MoQ session
// (which needs a live QUIC relay) and the ledger writer (which needs a store and
// track) — so it depends on small consumer-side interfaces at those seams rather
// than the concrete types. The MoQ seam cannot be [moqt.Session] directly: its
// Subscribe returns the concrete *moqt.TrackReader, which a test cannot build, so
// a thin adapter closes the gap and lets feedMedia's orchestration — skip a group
// that fails to read or package, advance the timeline only on a committed append,
// treat silence as the publisher leaving — run against a fake without a relay.

// mediaSubscriber subscribes to a media track and returns its group source.
type mediaSubscriber interface {
	SubscribeMedia(ctx context.Context, m mediaInfo) (groupFeeder, error)
}

// groupFeeder is a subscribed track: a stream of groups, closed when the feed ends.
type groupFeeder interface {
	AcceptGroup(ctx context.Context) (receivedGroup, error)
	Close() error
}

// receivedGroup is one MoQ group: its producer sequence and its frames, read one
// at a time until io.EOF.
type receivedGroup interface {
	GroupSequence() moqt.GroupSequence
	ReadFrame(*moqt.Frame) error
}

// groupAppender commits a packaged group. The ledger writer satisfies it; a fake
// records what the feed tried to append.
type groupAppender interface {
	AppendGroup(ctx context.Context, meta ledger.GroupInfo, payload []byte) (ledger.GroupInfo, error)
}

// epochAppender is a [groupAppender] whose epochs the caller closes: the
// announced-track loop starts a new epoch after each feed that wrote groups.
type epochAppender interface {
	groupAppender
	NewEpoch(ctx context.Context) error
}

// countingAppender counts the groups one feed committed, so the caller can
// tell a feed that wrote media from one that ended before writing any.
type countingAppender struct {
	groupAppender
	appended int
}

func (c *countingAppender) AppendGroup(ctx context.Context, meta ledger.GroupInfo, payload []byte) (ledger.GroupInfo, error) {
	info, err := c.groupAppender.AppendGroup(ctx, meta, payload)
	if err == nil {
		c.appended++
	}
	return info, err
}

// sessionSubscriber adapts [moqt.Session] to [mediaSubscriber].
type sessionSubscriber struct{ sess *moqt.Session }

func (s sessionSubscriber) SubscribeMedia(ctx context.Context, m mediaInfo) (groupFeeder, error) {
	tr, _, err := subscribeFollowing(ctx, s.sess, m.path, m.name)
	if err != nil {
		return nil, err
	}
	return trackFeeder{tr}, nil
}

// trackFeeder adapts [moqt.TrackReader] to [groupFeeder].
type trackFeeder struct{ tr *moqt.TrackReader }

func (t trackFeeder) AcceptGroup(ctx context.Context) (receivedGroup, error) {
	gr, err := t.tr.AcceptGroup(ctx)
	if err != nil {
		return nil, err
	}
	return moqtGroup{gr}, nil
}

func (t trackFeeder) Close() error { return t.tr.Close() }

// moqtGroup adapts [moqt.GroupReader] to [receivedGroup].
type moqtGroup struct{ gr *moqt.GroupReader }

func (g moqtGroup) GroupSequence() moqt.GroupSequence { return g.gr.GroupSequence() }
func (g moqtGroup) ReadFrame(f *moqt.Frame) error     { return g.gr.ReadFrame(f) }

// Compile-time checks that the adapters and the ledger writer satisfy the seams.
var (
	_ mediaSubscriber = sessionSubscriber{}
	_ groupFeeder     = trackFeeder{}
	_ receivedGroup   = moqtGroup{}
	_ groupAppender   = (*ledger.Writer)(nil)
	_ epochAppender   = (*ledger.Writer)(nil)
	_ groupAppender   = (*countingAppender)(nil)
)

// feedMedia subscribes to the media track and stores each received group in
// the ledger as a sealed group. It blocks until ctx is cancelled or the
// subscription fails.
//
// A group is stored as the publisher sent it — its frames in the moq-lite-05
// encoding (qumo-ledger decision 15) — and packaged into CMAF only when a
// viewer asks for it. Its timing comes from the LOC timestamps inside the
// frames, since publishers leave the moq-lite frame timestamp at zero: the
// gaps between frames, plus the last frame's duration, which this writer
// states as the group's mean interval.
func feedMedia(ctx context.Context, sub mediaSubscriber, m mediaInfo, w groupAppender, live *liveness, cfg feedConfig) error {
	src, err := sub.SubscribeMedia(ctx, m)
	if err != nil {
		return fmt.Errorf("hls: subscribe %s: %w", m.name, err)
	}
	defer src.Close()

	slog.Info("hls: subscribed", "path", m.path, "name", m.name)

	f := moqt.NewFrame(0)
	// mediaTime accumulates the real extents rather than multiplying an ordinal
	// by a fixed step, so a fragment that runs long or short moves the timeline
	// by what it actually contains. anchor is the wall clock at mediaTime zero.
	var mediaTime int64
	var anchor time.Time
	for {
		// Bound the wait for the next group. A publisher that goes away without
		// closing — a browser tab that navigated, a process killed — leaves this
		// blocked on a session that has not timed out yet, and while it blocks
		// the egress neither reconnects nor opens the epoch a restarted
		// publisher needs. Treating silence as the feed ending puts that
		// recovery on a clock the egress controls.
		acceptCtx, cancelAccept := context.WithTimeout(ctx, cfg.liveTimeout)
		gr, err := src.AcceptGroup(acceptCtx)
		cancelAccept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// DeadlineExceeded, not a bare acceptCtx.Err(): cancelAccept just
			// ran, so the context is already Canceled on every error path —
			// only an expired deadline distinguishes a silent publisher from a
			// real accept failure.
			if errors.Is(acceptCtx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("hls: no group for %s, treating the publisher as gone",
					cfg.liveTimeout)
			}
			return fmt.Errorf("hls: accept group: %w", err)
		}

		raw, err := drainGroup(gr, f)
		if err != nil {
			slog.Warn("hls: read group, skipping", "group", gr.GroupSequence(), "err", err)
			continue
		}
		if len(raw) == 0 {
			continue
		}
		payload, duration, err := storedGroup(raw)
		if err != nil {
			// A group that cannot be measured could not be packaged either.
			slog.Warn("hls: measure group, skipping", "group", gr.GroupSequence(), "err", err)
			continue
		}

		// The wall clock is read once, when the first group lands, and every
		// group after it is placed by its own media time. Reading the clock per
		// group would stamp arrival jitter onto the timeline instead: two
		// segments a second apart in media would carry whatever gap the network
		// happened to add, and EXT-X-PROGRAM-DATE-TIME would describe the
		// delivery rather than the media it names.
		now := time.Now()
		if anchor.IsZero() {
			anchor = now
		}
		// The ledger's running media time places the group, so the fragment
		// built from it and the segment listing cannot disagree about where it
		// belongs — and a group the ledger refuses below advances neither.
		info := groupInfo(uint64(gr.GroupSequence()), mediaTime, duration, uint64(len(raw)),
			wallclockAt(anchor, mediaTime, m.schema.Timescale))
		if _, err := w.AppendGroup(ctx, info, payload); err != nil {
			// A sealed group is immutable; a duplicate or ordering refusal is
			// skipped rather than stopping the feed.
			slog.Warn("hls: append group, skipping", "group", info.ID, "err", err)
			continue
		}
		// Marked only once the group is committed, so liveness means media a
		// client can actually fetch rather than media that merely arrived.
		live.mark(now)
		mediaTime += duration
	}
}

// drainRaw concatenates a group's frames verbatim. The catalog track carries
// one JSON document rather than media, so its frames are not LOC and are joined
// rather than decoded.
func drainRaw(gr *moqt.GroupReader, f *moqt.Frame) ([]byte, error) {
	var payload []byte
	for {
		if err := gr.ReadFrame(f); err != nil {
			if err == io.EOF {
				return payload, nil
			}
			return nil, err
		}
		payload = append(payload, f.Body()...)
	}
}

// drainGroup reads every frame of a group as it arrived: its moq-lite
// timestamp and its body. io.EOF ends the group cleanly.
func drainGroup(gr receivedGroup, f *moqt.Frame) ([]frame, error) {
	var frames []frame
	for {
		if err := gr.ReadFrame(f); err != nil {
			if err == io.EOF {
				return frames, nil
			}
			return nil, err
		}
		frames = append(frames, frame{
			Timestamp: f.Timestamp,
			// ReadFrame reuses its buffer, so the body is copied out before the
			// next read overwrites it.
			Body: bytes.Clone(f.Body()),
		})
	}
}

// storedGroup returns raw, a group's frames as received, as the payload the
// ledger stores, with the media extent it covers: from its first LOC
// timestamp to the end of its last frame, whose duration is the group's mean
// frame interval. A group whose frames are not LOC, or whose timestamps cannot
// be measured, is an error: the packager would refuse it on read.
func storedGroup(raw []frame) (payload []byte, duration int64, err error) {
	if len(raw) < 2 {
		return nil, 0, fmt.Errorf("hls: a single frame states no interval")
	}
	var first, prev uint64
	for i, f := range raw {
		ts, _, err := cmaf.DecodeLOC(f.Body)
		if err != nil {
			return nil, 0, fmt.Errorf("frame %d: %w", i, err)
		}
		if i == 0 {
			first = ts
		} else if ts <= prev {
			return nil, 0, fmt.Errorf("hls: frame %d timestamp %d does not advance past frame %d (%d)",
				i, ts, i-1, prev)
		}
		prev = ts
	}
	span := prev - first
	last := uint32(span / uint64(len(raw)-1))
	payload, err = encodeFrames(raw)
	if err != nil {
		return nil, 0, err
	}
	return payload, int64(span) + int64(last), nil
}

// groupInfo maps a MoQ group to a ledger [ledger.GroupInfo]. seq is the group's
// producer sequence (its identity, carried in the ID; the writer stamps the
// epoch); mediaTime is where this group starts on the track's timeline; and
// durationUnits is its extent, both in the track's timescale.
//
// Media time is the running sum of the groups actually appended rather than a
// function of the producer's sequence, because MoQ sequences are gappy — a
// dropped group must not leave a hole in the timeline.
func groupInfo(seq uint64, mediaTime, durationUnits int64, objectCount uint64, at time.Time) ledger.GroupInfo {
	return ledger.GroupInfo{
		ID:          ledger.NewGroupID(0, seq),
		MediaTime:   mediaTime,
		Duration:    durationUnits,
		Wallclock:   at.UnixNano(),
		ObjectCount: objectCount,
	}
}

// wallclockAt places a media offset on the wall clock, relative to the anchor
// taken when the feed's first group landed. Seconds and remainder are converted
// separately so a long session cannot overflow the nanosecond multiply.
func wallclockAt(anchor time.Time, mediaTime int64, timescale uint32) time.Time {
	if timescale == 0 {
		return anchor
	}
	ts := int64(timescale)
	seconds := mediaTime / ts
	remainder := mediaTime % ts
	return anchor.
		Add(time.Duration(seconds) * time.Second).
		Add(time.Duration(remainder * int64(time.Second) / ts))
}
