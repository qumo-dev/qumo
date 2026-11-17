package abr

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/qumo-dev/qumo/internal/envconfig"
	"github.com/qumo-dev/qumo/internal/optin"
	"github.com/qumo-dev/qumo/internal/quicconfig"
	"github.com/qumo-dev/qumo/internal/relaycred"
	"github.com/qumo-dev/qumo/internal/tlsclient"
	"github.com/qumo-dev/qumo/internal/upstream"
	"github.com/qumo-dev/qumo/token"
)

// retryInterval is how long to wait before dialing the relay again, or
// restarting a pipeline that failed while its source is still announced.
const retryInterval = 2 * time.Second

// Run starts the ABR transcoder. It connects to a relay, and for every
// broadcast a publisher opts in, by announcing it beneath token.ABRDirectory
// (/.abr/live/cam1 for /live/cam1) as qumo hls is opted into beneath /.hls/,
// publishes an ABR broadcast beside it, at the same path with ABR_SUFFIX
// appended (/live/cam1.abr): the source's tracks as they are, plus each
// rendition of the ladder shorter than the source video, encoded by ffmpeg.
// Every video track is in one MSF alternate group, and every rendition numbers
// its groups as the source does, so a player can switch between them at any
// group boundary.
//
// The opt-in carries no media: it redirects every subscription to the
// broadcast (see internal/optin), and the transcoder follows a redirect only
// there. A publisher opts in only what it may publish: a key confined to a
// prefix signs for the prefix's mirror beneath /.abr/ once its key set entry
// holds the abr opt-in (token.Key.OptIns).
//
// Configuration is read from environment variables (qumo convention):
//
//	RELAY_URL          - the relay, e.g. "https://relay:4433" (default "https://localhost:4433");
//	                     it may carry a credential as ?jwt= granting what RelayGrant
//	                     does, which lapses with it
//	RELAY_SIGNING_KEY  - a signing key (qumo auth keygen) the relay's key set trusts,
//	                     for the whole namespace (prefix "/"): the transcoder signs a
//	                     credential granting RelayGrant with it on every dial, in place
//	                     of any ?jwt=, and a fresh one on the session before it lapses
//	RELAY_CA_FILE      - PEM cert to trust as the relay's root (default: the system roots)
//	RELAY_TLS_INSECURE - skip relay TLS verification, for a self-signed dev relay (default "false")
//	ABR_SUFFIX         - appended to a source's path to name its ABR broadcast (default ".abr");
//	                     an opt-in for a broadcast already ending in it is ignored
//	ABR_LADDER         - the renditions, height:kbps, comma-separated (default "720:2500,360:800")
//	ABR_FFMPEG         - the ffmpeg executable (default "ffmpeg")
//	ABR_CODEC          - the ffmpeg H.264 encoder (default "libx264"; e.g. "h264_nvenc")
//	ABR_PRESET         - the encoder preset (default "veryfast"; empty for none)
//	QUIC_*             - QUIC transport settings of the session to the relay
//	                     (quicconfig.FromEnv lists them)
//
// The source video must be H.264 (avc1: AVCC with its configuration in the
// catalog, or Annex B with its parameter sets in the stream) or VP9, without
// B-frames. Renditions are H.264.
func Run(_ []string) error {
	ladder, err := ParseLadder(envconfig.String("ABR_LADDER", DefaultLadder))
	if err != nil {
		return err
	}
	cfg := Config{
		Ladder: ladder,
		Encoder: EncoderConfig{
			FFmpeg: envconfig.String("ABR_FFMPEG", "ffmpeg"),
			Codec:  envconfig.String("ABR_CODEC", "libx264"),
			Preset: os.Getenv("ABR_PRESET"),
		},
	}
	if _, ok := os.LookupEnv("ABR_PRESET"); !ok {
		cfg.Encoder.Preset = "veryfast"
	}
	if _, err := exec.LookPath(cfg.Encoder.FFmpeg); err != nil {
		return fmt.Errorf("abr: ABR_FFMPEG: %w", err)
	}
	suffix := envconfig.String("ABR_SUFFIX", ".abr")
	if suffix == "" {
		return errors.New("abr: ABR_SUFFIX must not be empty: the ABR broadcast would replace its source")
	}

	quicConfig, err := quicconfig.FromEnv(nil)
	if err != nil {
		return fmt.Errorf("abr: %w", err)
	}
	quicconfig.LogEnvSet("abr")
	relayURL := envconfig.String("RELAY_URL", "https://localhost:4433")
	tc := &tls.Config{MinVersion: tls.VersionTLS13}
	if err := tlsclient.Apply(tc, envconfig.String("RELAY_CA_FILE", ""), envconfig.String("RELAY_TLS_INSECURE", "false") == "true"); err != nil {
		return fmt.Errorf("abr: relay TLS: %w", err)
	}
	d := relayDialer{url: relayURL, tls: tc, quic: quicConfig}
	if keyFile := envconfig.String("RELAY_SIGNING_KEY", ""); keyFile != "" {
		key, err := token.LoadSigningKey(keyFile)
		if err != nil {
			return fmt.Errorf("abr: RELAY_SIGNING_KEY: %w", err)
		}
		if err := checkSigningKey(key); err != nil {
			return fmt.Errorf("abr: RELAY_SIGNING_KEY: %w", err)
		}
		d.key = &key
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	t := &Transcoder{
		Suffix: suffix,
		Config: cfg,
	}
	slog.Info("abr: starting", "relay", upstream.RelayName(relayURL), "opt_in", token.ABRDirectory, "suffix", suffix,
		"ladder", envconfig.String("ABR_LADDER", DefaultLadder), "codec", cfg.Encoder.Codec)
	for {
		// A TrackMux per session, so a session's ABR broadcasts end with it.
		mux := moqt.NewTrackMux(0)
		sess, err := d.dial(ctx, mux)
		if err == nil {
			slog.Info("abr: connected to the relay")
			err = t.Serve(ctx, sess, mux)
			// not actionable: the session is over either way
			_ = sess.CloseWithError(moqt.NoError, "abr stopping")
		}
		if ctx.Err() != nil {
			return nil
		}
		slog.Warn("abr: relay session ended, dialing again", "error", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retryInterval):
		}
	}
}

// RelayGrant is what the transcoder's session to the relay needs: listing
// and subscribing to the opt-ins beneath token.ABRDirectory and the
// broadcasts they name, and publishing the ABR broadcasts beside those. The
// opt-ins and the broadcasts lie anywhere, so the grant is the whole
// namespace.
func RelayGrant() token.Grant {
	return token.Grant{Root: "/", Subscribe: []string{"/"}, Publish: []string{"/"}}
}

// checkSigningKey reports why key cannot sign the transcoder's credential:
// confined to less than the whole namespace, which RelayGrant needs.
func checkSigningKey(key token.SigningKey) error {
	if p := key.Prefix; p != "" && p != "/" {
		return fmt.Errorf("the key is confined to %q, but the transcoder reads the opt-ins beneath %q and the broadcasts they name, and publishes beside them: use a key for the whole namespace (prefix \"/\")",
			p, token.ABRDirectory)
	}
	return nil
}

// relayDialer opens the transcoder's sessions to the relay.
type relayDialer struct {
	url  string
	tls  *tls.Config
	quic *quic.Config
	// key, when set, signs a credential granting RelayGrant for each
	// session in place of any ?jwt= in url, and a fresh one on the session
	// before its grant lapses.
	key *token.SigningKey
}

// dial opens a session to the relay serving mux.
func (d relayDialer) dial(ctx context.Context, mux *moqt.TrackMux) (*moqt.Session, error) {
	target, lapses, err := d.target()
	if err != nil {
		return nil, err
	}
	sess, err := (&moqt.Dialer{TLSConfig: d.tls.Clone(), QUICConfig: d.quic}).Dial(ctx, target, mux)
	if err != nil {
		return nil, fmt.Errorf("abr: dial relay: %w", err)
	}
	if d.key != nil {
		go relaycred.Keep(sess, *d.key, RelayGrant(), lapses)
	}
	return sess, nil
}

// target returns the URL to dial, carrying a freshly signed credential when
// the transcoder signs its own, and when that credential's grant lapses (zero
// for none).
func (d relayDialer) target() (string, time.Time, error) {
	if d.key == nil {
		return d.url, time.Time{}, nil
	}
	return relaycred.Target(d.url, *d.key, RelayGrant())
}

// A Transcoder publishes an ABR broadcast for every broadcast opted in
// beneath token.ABRDirectory.
type Transcoder struct {
	// Suffix is appended to a source's path to name its ABR broadcast.
	Suffix string
	// Config is what each pipeline makes of its source.
	Config Config
}

// Serve transcodes the broadcasts opted in beneath token.ABRDirectory on
// sess, publishing the ABR broadcasts on mux, the TrackMux sess was dialed
// with, until ctx or sess ends.
func (t *Transcoder) Serve(ctx context.Context, sess *moqt.Session, mux *moqt.TrackMux) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(sess.Context(), cancel)
	defer stop()

	ar, err := sess.AcceptAnnounce(token.ABRDirectory)
	if err != nil {
		return fmt.Errorf("abr: accept opt-ins: %w", err)
	}
	defer ar.Close() // not actionable: the session is ending

	var wg sync.WaitGroup
	defer wg.Wait()
	for ann := range ar.Announcements(ctx) {
		optIn := ann.BroadcastPath()
		src, ok := optedIn(optIn)
		if !ok {
			slog.Warn("abr: refusing opt-in: not spelled as its broadcast's opt-in", "opt_in", optIn, "broadcast", src)
			continue
		}
		if src == "/" || strings.HasSuffix(string(src), t.Suffix) {
			// No broadcast, or an ABR broadcast: it is not transcoded again.
			slog.Info("abr: ignoring opt-in", "opt_in", optIn)
			continue
		}
		dst := moqt.BroadcastPath(string(src) + t.Suffix)
		wg.Go(func() {
			annCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			stop := ann.AfterFunc(cancel)
			defer stop()
			t.transcode(annCtx, sess, mux, optIn, src, dst)
		})
	}
	return context.Cause(ctx)
}

// optedIn returns the broadcast the opt-in optIn names, and false when optIn
// is not that broadcast's opt-in exactly as optin.Path spells it. A relay
// tells broadcasts apart, and grants them, by their exact paths, and the
// transcoder may read every one: /.abr/acme/../victim/cam is granted beneath
// /.abr/acme/, yet would name /victim/cam, and a doubled or trailing slash
// names a broadcast other than the cleaned one the transcoder would read.
func optedIn(optIn moqt.BroadcastPath) (moqt.BroadcastPath, bool) {
	b, ok := optin.Broadcast(string(optIn), token.ABRDirectory)
	src := moqt.BroadcastPath(b)
	return src, ok && (src == "/" || optin.Path(token.ABRDirectory, src) == optIn)
}

// transcode runs the pipeline for src, opted in as optIn, until ctx ends,
// restarting it when it fails while the opt-in is still announced.
func (t *Transcoder) transcode(ctx context.Context, sess *moqt.Session, mux *moqt.TrackMux, optIn, src, dst moqt.BroadcastPath) {
	for {
		slog.Info("abr: transcoding", "opt_in", optIn, "source", src, "abr", dst)
		err := run(ctx, sess, mux, optIn, src, dst, t.Config)
		if ctx.Err() != nil {
			slog.Info("abr: opt-in ended", "opt_in", optIn)
			return
		}
		if mis, ok := errors.AsType[*MisdirectedError](err); ok {
			// Not worth retrying: the opt-in names another broadcast than
			// the one it would have transcoded.
			slog.Warn("abr: refusing opt-in", "error", mis)
			return
		}
		if err == nil {
			// The source ended its catalog while still announced; it is
			// restarting, or about to be unannounced.
			err = errors.New("source catalog ended")
		}
		slog.Warn("abr: pipeline stopped, restarting", "source", src, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryInterval):
		}
	}
}
