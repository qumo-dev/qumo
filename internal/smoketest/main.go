package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/qumo-dev/qumo/internal/tlsclient"
)

const (
	broadcastPath = "/smoke/test"
	trackName     = "data"
	// jwtEnv names the environment variable holding the credential for
	// relays with an auth server. It is read from the environment, not a
	// flag, so it stays out of the process list and shell history.
	jwtEnv = "RELAY_JWT"
)

func main() {
	pubURL := flag.String("pub", "", "publisher-side relay URL (e.g. moqt://localhost:9002); the credential comes from "+jwtEnv)
	subURL := flag.String("sub", "", "subscriber-side relay URL (e.g. moqt://localhost:9006); the credential comes from "+jwtEnv)
	caFile := flag.String("ca", "", "PEM file of the relays' TLS cert/CA to trust (required unless -insecure)")
	insecure := flag.Bool("insecure", false, "skip TLS verification (dev; self-signed relays)")
	timeout := flag.Duration("timeout", 30*time.Second, "overall test timeout")
	numGroups := flag.Int("groups", 5, "number of groups to send")
	numFrames := flag.Int("frames", 10, "number of frames per group")
	frameSize := flag.Int("framesize", 2048, "frame payload size in bytes")

	flag.Parse()

	if *pubURL == "" || *subURL == "" {
		fmt.Fprintln(os.Stderr, "both -pub and -sub flags are required")
		printUsage()
		os.Exit(1)
	}
	if *caFile == "" && !*insecure {
		fmt.Fprintln(os.Stderr, "either -ca <cert.pem> or -insecure is required")
		printUsage()
		os.Exit(1)
	}

	jwt := os.Getenv(jwtEnv)
	pub, err := parseRelayURL(*pubURL, jwt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: -pub:", err)
		os.Exit(1)
	}
	sub, err := parseRelayURL(*subURL, jwt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: -sub:", err)
		os.Exit(1)
	}

	tlsConf, err := smokeTLSConfig(*caFile, *insecure)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	os.Exit(run(ctx, pub, sub, *numGroups, *numFrames, *frameSize, tlsConf))
}

// printUsage writes the usage block to stderr.
func printUsage() {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  smoketest -pub <url> -sub <url> [-ca <cert.pem> | -insecure]")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Example:")
	fmt.Fprintln(os.Stderr, "  smoketest -pub moqt://localhost:9002 -sub moqt://localhost:9006 -insecure")
}

// relayURL is a relay URL from the command line, safe to log, and the
// credential added to it only when dialing.
type relayURL struct {
	u   *url.URL
	jwt string
}

// parseRelayURL parses raw, a relay URL without a query, to be dialed with
// jwt. A query is refused, since the credential comes from jwtEnv. It is
// checked before parsing, whose error quotes the input.
func parseRelayURL(raw, jwt string) (relayURL, error) {
	if strings.Contains(raw, "?") {
		return relayURL{}, errors.New("must not have a query; set the credential in " + jwtEnv)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return relayURL{}, err
	}
	return relayURL{u: u, jwt: jwt}, nil
}

// String returns the URL without its credential.
func (r relayURL) String() string {
	return r.u.String()
}

// dialURL returns the URL with the credential as its query.
func (r relayURL) dialURL() string {
	u := *r.u
	if r.jwt != "" {
		u.RawQuery = url.Values{"jwt": {r.jwt}}.Encode()
	}
	return u.String()
}

// smokeTLSConfig builds the TLS config shared by both dialers: verify against
// caFile's trust anchor, or skip verification when insecure (self-signed dev
// relays). Mirrors qumo's client TLS convention — verification is the default,
// -insecure is the explicit escape hatch. The trust decision is shared via
// [tlsclient.Apply]; this harness owns no base-config policy of its own.
func smokeTLSConfig(caFile string, insecure bool) (*tls.Config, error) {
	tc := &tls.Config{}
	if err := tlsclient.Apply(tc, caFile, insecure); err != nil {
		return nil, err
	}
	return tc, nil
}

func run(ctx context.Context, pubURL, subURL relayURL, numGroups, numFrames, frameSize int, tlsConf *tls.Config) int {
	testData := generateTestData(numGroups, numFrames, frameSize)
	sentHash := hashAllFlat(testData, numGroups, numFrames)

	// --- Publisher ---
	pubMux := moqt.NewTrackMux(0)

	pubMux.PublishFunc(ctx, moqt.BroadcastPath(broadcastPath), func(tw *moqt.TrackWriter) {
		defer tw.Close()
		for g := range numGroups {
			gw, err := tw.OpenGroup(ctx)
			if err != nil {
				log.Printf("publish: OpenGroup: %v", err)
				return
			}
			for f := range numFrames {
				frame := moqt.NewFrame(frameSize)
				if _, err := frame.Write(testData[g*numFrames+f]); err != nil {
					log.Printf("publish: frame.Write: %v", err)
					_ = gw.Close()
					return
				}
				if err := gw.WriteFrame(frame); err != nil {
					log.Printf("publish: WriteFrame: %v", err)
					_ = gw.Close()
					return
				}
			}
			if err := gw.Close(); err != nil {
				log.Printf("publish: Close group: %v", err)
				return
			}
		}
		log.Printf("publish: sent %d groups × %d frames ✓", numGroups, numFrames)
	})

	pubDialer := &moqt.Dialer{TLSConfig: tlsConf}
	pubSess, err := pubDialer.Dial(ctx, pubURL.dialURL(), pubMux)
	if err != nil {
		log.Printf("publish: dial %s: %v", pubURL, err)
		return 1
	}
	defer pubSess.CloseWithError(moqt.NoError, "done")
	log.Printf("publish: connected to %s", pubURL)

	// Wait for announcement to propagate across relay mesh.
	select {
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
		log.Printf("timeout waiting for propagation")
		return 1
	}

	// --- Subscriber ---
	subMux := moqt.NewTrackMux(0)
	subDialer := &moqt.Dialer{TLSConfig: tlsConf}
	subSess, err := subDialer.Dial(ctx, subURL.dialURL(), subMux)
	if err != nil {
		log.Printf("subscribe: dial %s: %v", subURL, err)
		return 1
	}
	defer subSess.CloseWithError(moqt.NoError, "done")
	log.Printf("subscribe: connected to %s", subURL)

	tr, err := subSess.Subscribe(ctx,
		moqt.BroadcastPath(broadcastPath),
		moqt.TrackName(trackName), nil)
	if err != nil {
		log.Printf("subscribe: Subscribe: %v", err)
		return 1
	}
	defer tr.Close()
	log.Println("subscribe: subscribed, reading groups...")

	// Read all groups/frames and compute hash.
	h := sha256.New()
	groupCount := 0
	frameCount := 0
	buf := moqt.NewFrame(frameSize + 256)

	for groupCount < numGroups {
		gr, err := tr.AcceptGroup(ctx)
		if err != nil {
			log.Printf("subscribe: AcceptGroup: %v", err)
			break
		}
		for frame := range gr.Frames(buf) {
			h.Write(frame.Body())
			frameCount++
		}
		groupCount++
	}

	recvHash := fmt.Sprintf("%x", h.Sum(nil))

	log.Printf("subscribe: received %d groups, %d frames ✓", groupCount, frameCount)

	if recvHash != sentHash {
		fmt.Println("")
		fmt.Printf("❌ FAIL: connectivity check failed — hash mismatch\n   sent=%s\n   recv=%s\n", sentHash, recvHash)
		return 1
	}

	fmt.Println("")
	fmt.Printf("📡 PASS: %d groups × %d frames streamed end-to-end\n   %s → %s\n",
		numGroups, numFrames, pubURL, subURL)
	return 0
}

// generateTestData creates deterministic test payloads.
func generateTestData(numGroups, numFrames, frameSize int) [][]byte {
	data := make([][]byte, numGroups*numFrames)
	var buf []byte
	for g := range numGroups {
		for f := range numFrames {
			buf = append(buf[:0], "group="...)
			buf = strconv.AppendInt(buf, int64(g), 10)
			buf = append(buf, " frame="...)
			buf = strconv.AppendInt(buf, int64(f), 10)
			buf = append(buf, ' ')

			payload := bytes.Repeat(buf, (frameSize/len(buf))+1)[:frameSize]
			data[g*numFrames+f] = payload
		}
	}
	return data
}

// hashAllFlat computes SHA-256 over all payloads in group×frame order.
func hashAllFlat(data [][]byte, numGroups, numFrames int) string {
	h := sha256.New()
	for g := range numGroups {
		for f := range numFrames {
			h.Write(data[g*numFrames+f])
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
