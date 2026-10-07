package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"

	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/internal/doctor"
	"github.com/qumo-dev/qumo/internal/hls"
	"github.com/qumo-dev/qumo/internal/ingest"
	"github.com/qumo-dev/qumo/internal/loadgen"
	"github.com/qumo-dev/qumo/internal/playground"
	"github.com/qumo-dev/qumo/internal/relay"
	"github.com/qumo-dev/qumo/internal/update"
	"github.com/qumo-dev/qumo/internal/version"
)

var (
	// overridable command handlers for easier unit-testing
	runHLS        = hls.Run
	runRelay      = relay.Run
	runRTMP       = ingest.RunRTMP
	runRTSP       = ingest.RunRTSP     // push server (ANNOUNCE/RECORD)
	runRTSPPull   = ingest.RunRTSPPull // pull client (DESCRIBE/SETUP/PLAY, camera ingest)
	runAuth       = auth.Run
	runDoctor     = doctor.Run
	runLoadgen    = loadgen.Run
	runUpdate     = update.Run
	runPlayground = func(args []string) error {
		o, err := playground.ParseFlags(args, playground.Options{
			Assets:     mustSubAssets(),
			StartRelay: func() error { return runRelay(nil) },
		})
		if err != nil {
			if playground.IsErrHelp(err) {
				// -h/--help: print usage to stdout, exit 0.
				playground.UsageHelp(os.Stdout)
				return nil
			}
			// Bad flag / unexpected arg: surface the error plus a usage hint.
			playground.UsageHelp(os.Stderr)
			return err
		}
		return playground.Run(context.Background(), o)
	}
)

// mustSubAssets returns the embedded playground UI rooted at the dist content
// root. The embed is declared in embed.go; fs.Sub relocates files so they
// appear at the FS root (index.html, assets/...) as the playground package
// expects. A panic here is unreachable: the compile-time embed guarantees the
// "playground/dist" subtree exists.
func mustSubAssets() fs.FS {
	sub, err := fs.Sub(playgroundAssets, "playground/dist")
	if err != nil {
		panic(fmt.Sprintf("qumo: embed playground/dist: %v", err))
	}
	return sub
}

func main() {
	os.Exit(run(os.Args[1:]))
}

// run executes the command logic and returns an exit code (0 = success).
// Keeping this function small makes unit-testing straightforward.
func run(args []string) int {
	if len(args) < 1 {
		printUsage()
		return 1
	}

	cmd := args[0]
	cmdArgs := args[1:]

	// Handle version flags anywhere in args
	if cmd == "version" || cmd == "--version" || cmd == "-v" {
		fmt.Println(version.Full())
		return 0
	}

	var err error
	switch cmd {
	case "auth":
		err = runAuth(cmdArgs)
	case "hls":
		err = runHLS(cmdArgs)
	case "relay":
		err = runRelay(cmdArgs)
	case "rtmp":
		err = runRTMP(cmdArgs)
	case "rtsp":
		err = runRTSPPull(cmdArgs)
	case "rtsp-push":
		err = runRTSP(cmdArgs)
	case "playground":
		err = runPlayground(cmdArgs)
	case "doctor":
		err = runDoctor(cmdArgs)
	case "loadgen":
		err = runLoadgen(cmdArgs)
	case "update":
		err = runUpdate(cmdArgs)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		printUsage()
		return 1
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "Usage: qumo <command>  (%s)\n", version.Short())
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Commands:")
	fmt.Fprintln(os.Stderr, "  auth       Set up the credentials a relay verifies (keygen, token)")
	fmt.Fprintln(os.Stderr, "  hls        Start the HLS/DASH egress server")
	fmt.Fprintln(os.Stderr, "  relay      Start the MoQ relay server (--role hub|edge; default flat)")
	fmt.Fprintln(os.Stderr, "  rtmp       Start the RTMP ingest server")
	fmt.Fprintln(os.Stderr, "  rtsp       Pull from an RTSP source (e.g. IP camera) and republish as MoQT")
	fmt.Fprintln(os.Stderr, "  rtsp-push  Start the RTSP push ingest server (ANNOUNCE/RECORD)")
	fmt.Fprintln(os.Stderr, "  playground Start a local demo (relay + web UI) on http://127.0.0.1:8080")
	fmt.Fprintln(os.Stderr, "  doctor     Explain effective runtime config (GC target) — read-only")
	fmt.Fprintln(os.Stderr, "  loadgen    Drive an out-of-process capacity load against a relay (subscribe|publish)")
	fmt.Fprintln(os.Stderr, "  update     Update qumo to the latest release (--check to just check)")
	fmt.Fprintln(os.Stderr, "  version    Print version information")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Configuration:")
	fmt.Fprintln(os.Stderr, "  All commands are configured via environment variables.")
	fmt.Fprintln(os.Stderr, "  See relay-config.example.env for details.")
}
