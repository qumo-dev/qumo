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
	"github.com/qumo-dev/qumo/internal/optin"
	"github.com/qumo-dev/qumo/internal/quicconfig"
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
// prefix also signs for the prefix's mirror beneath /.abr/.
//
// Configuration is read from environment variables (qumo convention):
//
//	RELAY_URL          - the relay, e.g. "https://relay:4433" (default "https://localhost:4433");
//	                     it may carry a credential as ?jwt= that grants listing and
//	                     subscribing to the opt-ins beneath /.abr/ and the broadcasts
//	                     they name, and publishing the ABR broadcasts
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
		src := moqt.BroadcastPath(optin.Broadcast(string(optIn), token.ABRDirectory))
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
