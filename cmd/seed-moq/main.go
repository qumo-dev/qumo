// Command seed-moq runs a minimal MoQ server that publishes a live CMAF (fMP4)
// track, so the HLS egress (`qumo hls`) can be exercised end-to-end — MoQ
// catalog + CMAF groups into the ledger, served back as HLS — without a full
// relay and publisher.
//
// It serves the MSF catalog for a VP9 placeholder track on the reserved
// "catalog" track — VP9 carries its configuration in the codec string, so the
// catalog needs no fMP4 init in initData — and publishes one placeholder group
// per interval: a run of LOC frames the egress packages into a CMAF fragment.
// Payloads are placeholder bytes, so the playlist is structurally valid (and
// carries #EXT-X-MAP) but the segments are not decodable; real fMP4 comes from a
// WebCodecs publisher.
//
// It generates an ephemeral self-signed certificate, which the egress will not
// trust by default — point it at the seeder with RELAY_TLS_INSECURE=true.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/gomoqt/msf"

	"github.com/qumo-dev/qumo/internal/cmaf"
	"github.com/qumo-dev/qumo/internal/envconfig"
	"github.com/qumo-dev/qumo/internal/quicconfig"
)

func main() {
	addr := envconfig.String("SEED_ADDR", ":4433")
	path := envconfig.String("SEED_TRACK_PATH", "/live/cam1")
	trackName := envconfig.String("SEED_TRACK_NAME", "video")
	interval := envDuration("SEED_GROUP_INTERVAL", 2*time.Second)
	timescale := envIntOr("SEED_TIMESCALE", 90000)

	tlsConfig, err := selfSignedTLS()
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed-moq:", err)
		os.Exit(1)
	}
	tlsConfig.NextProtos = []string{"h3", moqt.NextProtoMOQ}
	quicConfig, err := quicconfig.FromEnv(nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed-moq:", err)
		os.Exit(1)
	}
	quicconfig.LogEnvSet("seed-moq")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	mux := moqt.NewTrackMux(0)

	broadcast, err := msf.NewBroadcast(msf.Catalog{Version: 1})
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed-moq:", err)
		os.Exit(1)
	}
	// The catalog states the track's packaging (CMAF/fMP4) and codec; the egress
	// builds the ledger schema and the HLS init segment from it. VP9 is the
	// placeholder codec because it carries its configuration in the codec string
	// and needs no out-of-band init — an AVC placeholder would be rejected at the
	// egress for lacking parameter sets. The picture size is required: the
	// packager refuses a track that states none.
	if err := broadcast.RegisterTrack(msf.Track{
		Name:      trackName,
		Packaging: msf.PackagingCMAF,
		Timescale: new(int64(timescale)),
		Codec:     "vp09.00.10.08",
		Width:     new(int64(1280)),
		Height:    new(int64(720)),
		Role:      msf.RoleVideo,
		IsLive:    new(true),
	}, moqt.TrackHandlerFunc(func(tw *moqt.TrackWriter) {
		publish(ctx, tw, interval)
	})); err != nil {
		fmt.Fprintln(os.Stderr, "seed-moq:", err)
		os.Exit(1)
	}

	// The broadcast is itself a TrackHandler: it serves the catalog track and
	// dispatches media-track subscribes to the registered handler.
	mux.Publish(ctx, moqt.BroadcastPath(path), broadcast)

	httpMux := http.NewServeMux()
	httpMux.Handle("/", &moqt.WebTransportHandler{
		TrackMux:    mux,
		CheckOrigin: func(*http.Request) bool { return true },
		Handler: moqt.HandleFunc(func(sess *moqt.Session) {
			defer sess.CloseWithError(moqt.NoError, moqt.NoError.String())
			<-sess.Context().Done()
		}),
	})

	srv := &moqt.Server{
		Addr:               addr,
		TLSConfig:          tlsConfig,
		QUICConfig:         quicConfig,
		WebTransportServer: moqt.NewWebTransportServer(httpMux),
		TrackMux:           mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		// not actionable: a shutdown error only reports a closed listener on exit.
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("seed-moq: serving", "addr", addr, "path", path, "track", trackName, "interval", interval)
	if err := srv.ListenAndServe(); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "seed-moq:", err)
		os.Exit(1)
	}
}

// framesPerGroup and frameInterval shape each placeholder group like a 30 fps
// GOP. The egress's packager needs at least two frames with advancing timestamps
// to measure sample durations, so a one-frame group would be skipped; a run of
// frames one interval apart gives it a real (placeholder) extent to package.
const (
	framesPerGroup = 30
	frameInterval  = 33_333 // ~30 fps, in microseconds (the LOC/CMAF timescale)
)

// publish streams a live track to one subscriber: one placeholder group every
// interval, each a run of LOC frames the egress packages into a CMAF fragment.
// It returns when ctx is cancelled or the subscriber disconnects.
func publish(ctx context.Context, tw *moqt.TrackWriter, interval time.Duration) {
	slog.Info("seed-moq: subscriber connected")
	defer slog.Info("seed-moq: subscriber gone")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var group uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-tw.Context().Done():
			return
		case <-ticker.C:
		}

		gw, err := tw.OpenGroupAt(ctx, moqt.GroupSequence(group))
		if err != nil {
			return
		}
		// Timestamps advance within the group so the packager can derive sample
		// durations from the gaps. Their absolute value is irrelevant — the
		// egress places each fragment by its own running media time, not the
		// capture clock — but they must strictly increase.
		base := group * framesPerGroup * frameInterval
		for i := range framesPerGroup {
			payload := []byte(fmt.Sprintf("g%d-f%d", group, i))
			if err := gw.WriteFrame(locFrame(base+uint64(i)*frameInterval, payload)); err != nil {
				// not actionable: the frame write already failed; closing abandons this group regardless.
				_ = gw.Close()
				return
			}
		}
		// not actionable: a failed close drops this group; the seeder publishes the next one.
		_ = gw.Close()
		group++
	}
}

// locFrame builds one LOC frame — the wire format the egress decodes — from a
// microsecond timestamp and a payload, then wraps it in a moqt.Frame. The
// seeder is not an encoder; the payload is placeholder bytes, but the container
// is real so the egress's LOC decoder and duration math read it and the group
// reaches the ledger instead of being skipped as undecodable.
func locFrame(timestamp uint64, payload []byte) *moqt.Frame {
	b := cmaf.EncodeLOC(timestamp, payload)
	f := moqt.NewFrame(len(b))
	// not actionable: Write only fails on a nil frame, which NewFrame is not.
	_, _ = f.Write(b)
	return f
}

// selfSignedTLS builds an ephemeral self-signed certificate valid for localhost
// and the loopback IPs, so the server runs with no on-disk cert material.
func selfSignedTLS() (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}},
		MinVersion:   tls.VersionTLS13,
	}, nil
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v == "" {
		return def
	} else if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Duration(n) * time.Second
	}
	return def
}
