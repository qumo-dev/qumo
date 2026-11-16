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

	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/qumo-dev/qumo/internal/envconfig"
	"github.com/qumo-dev/qumo/internal/quicconfig"
	"github.com/qumo-dev/qumo/internal/tlsclient"
	"github.com/qumo-dev/qumo/internal/upstream"
)

// retryInterval is how long to wait before dialing the relay again, or
// restarting a pipeline that failed while its source is still announced.
const retryInterval = 2 * time.Second

// Run starts the ABR transcoder. It connects to a relay, and for every
// broadcast announced beneath ABR_PREFIX publishes an ABR broadcast beside it,
// at the same path with ABR_SUFFIX appended (/live/cam1 → /live/cam1.abr): the
// source's tracks as they are, plus each rendition of the ladder shorter than
// the source video, encoded by ffmpeg. Every video track is in one MSF
// alternate group, and every rendition numbers its groups as the source does,
// so a player can switch between them at any group boundary.
//
// Configuration is read from environment variables (qumo convention):
//
//	RELAY_URL          - the relay, e.g. "https://relay:4433" (default "https://localhost:4433");
//	                     it may carry a credential as ?jwt= that grants subscribing
//	                     to the sources and publishing the ABR broadcasts
//	RELAY_CA_FILE      - PEM cert to trust as the relay's root (default: the system roots)
//	RELAY_TLS_INSECURE - skip relay TLS verification, for a self-signed dev relay (default "false")
//	ABR_PREFIX         - the directory whose broadcasts are transcoded (default "/")
//	ABR_SUFFIX         - appended to a source's path to name its ABR broadcast (default ".abr");
//	                     broadcasts already ending in it are not transcoded again, nor are
//	                     aliases: broadcasts that redirect subscribers to another, such as
//	                     HLS opt-ins (only the broadcast they name is)
//	ABR_LADDER         - the renditions, height:kbps, comma-separated (default "720:2500,360:800")
//	ABR_FFMPEG         - the ffmpeg executable (default "ffmpeg")
//	ABR_CODEC          - the ffmpeg H.264 encoder (default "libx264"; e.g. "h264_nvenc")
//	ABR_PRESET         - the encoder preset (default "veryfast"; empty for none)
//	QUIC_*             - QUIC transport settings of the session to the relay
//	                     (quicconfig.FromEnv lists them)
//
// The source video must be H.264 (avc1) without B-frames.
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
	prefix := envconfig.String("ABR_PREFIX", "/")
	if !strings.HasPrefix(prefix, "/") {
		return fmt.Errorf("abr: ABR_PREFIX %q must start with /", prefix)
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	t := &Transcoder{
		Prefix: prefix,
		Suffix: suffix,
		Config: cfg,
	}
	slog.Info("abr: starting", "relay", upstream.RelayName(relayURL), "prefix", prefix, "suffix", suffix,
		"ladder", envconfig.String("ABR_LADDER", DefaultLadder), "codec", cfg.Encoder.Codec)
	for {
		// A TrackMux per session, so a session's ABR broadcasts end with it.
		mux := moqt.NewTrackMux(0)
		sess, err := (&moqt.Dialer{TLSConfig: tc.Clone(), QUICConfig: quicConfig}).Dial(ctx, relayURL, mux)
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

// A Transcoder publishes an ABR broadcast for every broadcast announced
// beneath Prefix.
type Transcoder struct {
	// Prefix is the directory whose broadcasts are transcoded.
	Prefix string
	// Suffix is appended to a source's path to name its ABR broadcast.
	Suffix string
	// Config is what each pipeline makes of its source.
	Config Config
}

// Serve transcodes the broadcasts sess announces beneath Prefix, publishing the
// ABR broadcasts on mux, the TrackMux sess was dialed with, until ctx or sess
// ends.
func (t *Transcoder) Serve(ctx context.Context, sess *moqt.Session, mux *moqt.TrackMux) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(sess.Context(), cancel)
	defer stop()

	ar, err := sess.AcceptAnnounce(t.Prefix)
	if err != nil {
		return fmt.Errorf("abr: accept announcements: %w", err)
	}
	defer ar.Close() // not actionable: the session is ending

	var wg sync.WaitGroup
	defer wg.Wait()
	for ann := range ar.Announcements(ctx) {
		src := ann.BroadcastPath()
		if strings.HasSuffix(string(src), t.Suffix) {
			continue // an ABR broadcast, perhaps this transcoder's own
		}
		dst := moqt.BroadcastPath(string(src) + t.Suffix)
		wg.Go(func() {
			annCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			stop := ann.AfterFunc(cancel)
			defer stop()
			t.transcode(annCtx, sess, mux, src, dst)
		})
	}
	return context.Cause(ctx)
}

// transcode runs the pipeline for src until ctx ends, restarting it when it
// fails while src is still announced.
func (t *Transcoder) transcode(ctx context.Context, sess *moqt.Session, mux *moqt.TrackMux, src, dst moqt.BroadcastPath) {
	for {
		slog.Info("abr: transcoding", "source", src, "abr", dst)
		err := run(ctx, sess, mux, src, dst, t.Config)
		if ctx.Err() != nil {
			slog.Info("abr: source ended", "source", src)
			return
		}
		if alias, ok := errors.AsType[*AliasError](err); ok {
			slog.Info("abr: not transcoding an alias", "source", src, "redirects_to", alias.Target)
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
