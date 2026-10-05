package auth

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/qumo-dev/qumo/token"
)

const (
	defaultAddr     = "127.0.0.1:4440"
	shutdownTimeout = 10 * time.Second
	headerTimeout   = 5 * time.Second
)

const usage = `Usage: qumo auth <command>

The auth server a relay asks about every session (QUMO_AUTH_URL), and the
tools to set one up. An app signs a capability token per client with its own
Ed25519 key; the client connects with it (?jwt=…); the auth server verifies it.

Commands:
  keygen   Generate a signing key pair: the app's private key, and the public
           key set the auth server trusts
  serve    Run the auth server, configured by QUMO_AUTH_ADDR,
           QUMO_AUTH_KEYS_FILE and QUMO_AUTH_ANONYMOUS
  token    Sign a token by hand, for testing

Run "qumo auth <command> -h" for a command's flags.
`

// Run executes "qumo auth" with args, the arguments after "auth".
func Run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("qumo auth: a command is required")
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "serve":
		return runServe(rest)
	case "keygen":
		return runKeygen(rest, os.Stdout)
	case "token":
		return runToken(rest, os.Stdout)
	case "-h", "--help", "help":
		_, err := fmt.Fprint(os.Stdout, usage)
		return err
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("qumo auth: unknown command %q", cmd)
	}
}

// serveConfig is the auth server's configuration.
type serveConfig struct {
	addr      string
	keysFile  string
	anonymous []string
}

// loadServeConfig reads the environment:
//
//	QUMO_AUTH_ADDR      - listen address (default 127.0.0.1:4440, loopback:
//	                      the relay beside it is the only client)
//	QUMO_AUTH_KEYS_FILE - JWK Set of the trusted Ed25519 public keys, each
//	                      with an optional "prefix" member (qumo auth keygen
//	                      writes one)
//	QUMO_AUTH_ANONYMOUS - comma-separated subtree patterns ("anon/**") that a
//	                      session with no credential may publish and subscribe
//	                      to; "**" opens everything, for development only
//
// At least one of QUMO_AUTH_KEYS_FILE and QUMO_AUTH_ANONYMOUS must be set.
func loadServeConfig() (serveConfig, error) {
	cfg := serveConfig{
		addr:     os.Getenv("QUMO_AUTH_ADDR"),
		keysFile: os.Getenv("QUMO_AUTH_KEYS_FILE"),
	}
	if cfg.addr == "" {
		cfg.addr = defaultAddr
	}
	if raw := os.Getenv("QUMO_AUTH_ANONYMOUS"); raw != "" {
		anonymous, err := parsePatternList(raw)
		if err != nil {
			return serveConfig{}, fmt.Errorf("QUMO_AUTH_ANONYMOUS: %w", err)
		}
		cfg.anonymous = anonymous
	}
	if cfg.keysFile == "" && len(cfg.anonymous) == 0 {
		return serveConfig{}, errors.New("neither QUMO_AUTH_KEYS_FILE nor QUMO_AUTH_ANONYMOUS is set: " +
			"set QUMO_AUTH_KEYS_FILE to the trusted signing keys, QUMO_AUTH_ANONYMOUS to what sessions " +
			"without a credential may use, or both")
	}
	return cfg, nil
}

// parsePatternList parses comma-separated subtree patterns, as a grant
// carries them, and returns them trimmed.
func parsePatternList(raw string) ([]string, error) {
	var patterns []string
	for s := range strings.SplitSeq(raw, ",") {
		s = strings.TrimSpace(s)
		if _, err := parsePattern(s); err != nil {
			return nil, err
		}
		patterns = append(patterns, s)
	}
	return patterns, nil
}

func runServe(args []string) error {
	// serve takes no flags: like the relay, it is configured by the
	// environment (loadServeConfig).
	fs := flag.NewFlagSet("qumo auth serve", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadServeConfig()
	if err != nil {
		return err
	}
	var keys map[string]token.Key
	if cfg.keysFile != "" {
		if keys, err = token.LoadKeySet(cfg.keysFile); err != nil {
			return fmt.Errorf("QUMO_AUTH_KEYS_FILE: %w", err)
		}
	}

	mux := http.NewServeMux()
	// Static keys never change, so a relay has no reason to revalidate.
	mux.Handle("/", &Handler{Keys: keys, Anonymous: cfg.anonymous})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Addr: cfg.addr, Handler: mux, ReadHeaderTimeout: headerTimeout}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	slog.Info("auth server: listening", "addr", cfg.addr, "keys", len(keys), "keys_file", cfg.keysFile,
		"anonymous", cfg.anonymous)

	select {
	case err := <-serveErr:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// runKeygen writes a new signing key pair: the private key, for the app that
// signs tokens, and a key set holding its public key, for the auth server.
func runKeygen(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("qumo auth keygen", flag.ContinueOnError)
	prefix := fs.String("prefix", "", "confine the key to this path prefix (e.g. acme/app); empty for none")
	privPath := fs.String("out", "signing-key.jwk", "where to write the private signing key (keep it on the app's server)")
	pubPath := fs.String("keys", "keys.json", "where to write the public key set (QUMO_AUTH_KEYS_FILE)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	key, err := token.GenerateKey(*prefix)
	if err != nil {
		return err
	}
	priv, err := key.MarshalJWK()
	if err != nil {
		return err
	}
	set, err := token.MarshalKeySet(key.Public())
	if err != nil {
		return err
	}
	// O_EXCL: never overwrite an existing key, which would strand every token
	// signed with it.
	if err := writeNew(*privPath, priv, 0o600); err != nil {
		return err
	}
	if err := writeNew(*pubPath, set, 0o644); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "kid:        %s\nprefix:      %q\nsigning key: %s (private: keep it on the app's server)\nkey set:     %s (set QUMO_AUTH_KEYS_FILE to it)\n",
		key.ID, key.Prefix, *privPath, *pubPath)
	return err
}

// writeNew writes data to a new file at path, refusing to replace one. It
// creates the file through an os.Root on path's directory, as readFile reads.
func writeNew(path string, data []byte, perm os.FileMode) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer func() { _ = root.Close() }() // not actionable: the file itself is closed and checked below
	f, err := root.OpenFile(filepath.Base(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close() // not actionable: the write error is returned
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// runToken signs a token with a signing key, for testing a relay by hand.
func runToken(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("qumo auth token", flag.ContinueOnError)
	keyPath := fs.String("key", "signing-key.jwk", "the private signing key (qumo auth keygen)")
	publish := fs.String("publish", "", "the path the bearer may publish at or beneath")
	subscribe := fs.String("subscribe", "", "the path the bearer may subscribe at or beneath")
	ttl := fs.Duration("ttl", time.Hour, "how long the token is valid (at most 1h)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	key, err := token.LoadSigningKey(*keyPath)
	if err != nil {
		return err
	}
	tok, err := token.Sign(key, token.Grant{Publish: *publish, Subscribe: *subscribe}, *ttl)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, tok)
	return err
}
