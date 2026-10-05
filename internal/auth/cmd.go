package auth

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
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

const usage = `Usage: qumo auth [command]

With no command, runs the auth server a relay asks about every session
(QUMO_AUTH_URL), configured by QUMO_AUTH_KEYS_FILE and QUMO_AUTH_ADDR. An app
signs a capability token per client with its own Ed25519 key; the client
connects with it (?jwt=…); the auth server verifies it.

Commands:
  keygen   Generate a signing key pair: the app's private key, and the public
           key set the auth server trusts
  token    Sign a token by hand, for testing

Run "qumo auth <command> -h" for a command's flags.
`

// Run executes "qumo auth" with args, the arguments after "auth". With none,
// it runs the auth server, as "qumo relay" runs the relay.
func Run(args []string) error {
	if len(args) == 0 {
		return serve()
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "keygen":
		return runKeygen(rest, os.Stdout)
	case "token":
		return runToken(rest, os.Stdout, os.Stderr)
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
	addr     string
	keysFile string
}

// loadServeConfig reads the environment:
//
//	QUMO_AUTH_KEYS_FILE - required: JWK Set of the trusted Ed25519 public
//	                      keys, each with an optional "prefix" member (qumo
//	                      auth keygen writes one)
//	QUMO_AUTH_ADDR      - listen address (default 127.0.0.1:4440, loopback:
//	                      the relay beside it is the only client)
//
// A session without a token is refused. For a relay open to everyone, leave
// its QUMO_AUTH_URL unset instead.
func loadServeConfig() (serveConfig, error) {
	cfg := serveConfig{
		addr:     os.Getenv("QUMO_AUTH_ADDR"),
		keysFile: os.Getenv("QUMO_AUTH_KEYS_FILE"),
	}
	if cfg.addr == "" {
		cfg.addr = defaultAddr
	}
	if cfg.keysFile == "" {
		return serveConfig{}, errors.New("QUMO_AUTH_KEYS_FILE is not set: set it to the key set " +
			"\"qumo auth keygen\" wrote (keys.json)")
	}
	return cfg, nil
}

// serve runs the auth server until SIGINT or SIGTERM. Like the relay, it is
// configured by the environment (loadServeConfig).
func serve() error {
	cfg, err := loadServeConfig()
	if err != nil {
		return err
	}
	keys, err := token.LoadKeySet(cfg.keysFile)
	if err != nil {
		return fmt.Errorf("QUMO_AUTH_KEYS_FILE: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", &Handler{Keys: keys})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Addr: cfg.addr, Handler: mux, ReadHeaderTimeout: headerTimeout}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	if err := writeBanner(os.Stderr, cfg, keys); err != nil {
		return err
	}
	slog.Info("auth server: listening", "addr", cfg.addr, "keys", len(keys))

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

// writeBanner prints what the server trusts and how a relay reaches it, as
// the relay prints its endpoints at startup.
func writeBanner(w io.Writer, cfg serveConfig, keys map[string]token.Key) error {
	var b strings.Builder
	url := "http://" + cfg.addr
	fmt.Fprintf(&b, "qumo auth server\n")
	fmt.Fprintf(&b, "  %-11s %s\n", "Listen:", url)
	fmt.Fprintf(&b, "  %-11s QUMO_AUTH_URL=%s\n", "Relay:", url)
	fmt.Fprintf(&b, "  %-11s %d from %s\n", "Keys:", len(keys), cfg.keysFile)
	for _, kid := range slices.Sorted(maps.Keys(keys)) {
		fmt.Fprintf(&b, "  %-11s   %s  %s\n", "", kid, prefixLabel(keys[kid].Prefix))
	}
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// prefixLabel describes what a key with prefix may grant, for the terminal.
func prefixLabel(prefix string) string {
	if prefix == "" {
		return "any path (no prefix)"
	}
	return prefix + "/**"
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
		// Remove the private key just written, so a retry isn't refused by
		// a key whose public half was never saved.
		return errors.Join(err, removeFile(*privPath))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Generated an Ed25519 signing key.\n\n")
	fmt.Fprintf(&b, "  %-13s %s\n", "Key ID:", key.ID)
	fmt.Fprintf(&b, "  %-13s %s\n", "Grants:", prefixLabel(key.Prefix))
	fmt.Fprintf(&b, "  %-13s %s  (private: keep it on your app's server)\n", "Signing key:", *privPath)
	fmt.Fprintf(&b, "  %-13s %s  (public: for the auth server)\n", "Key set:", *pubPath)
	fmt.Fprintf(&b, "\nNext:\n")
	fmt.Fprintf(&b, "  1. Run the auth server:   QUMO_AUTH_KEYS_FILE=%s qumo auth\n", *pubPath)
	fmt.Fprintf(&b, "  2. Point the relay at it: QUMO_AUTH_URL=http://%s qumo relay\n", defaultAddr)
	fmt.Fprintf(&b, "  3. Sign a test token:     qumo auth token -key %s -publish %s\n", *privPath, examplePath(key.Prefix))
	_, err = io.WriteString(out, b.String())
	return err
}

// examplePath is a path a key with prefix may grant, for a usage example.
func examplePath(prefix string) string {
	if prefix == "" {
		return "demo"
	}
	return prefix + "/demo"
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

// removeFile removes path through an os.Root on its directory, as writeNew
// creates it.
func removeFile(path string) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	defer func() { _ = root.Close() }() // not actionable: the removal is checked below
	if err := root.Remove(filepath.Base(path)); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// runToken signs a token with a signing key, for testing a relay by hand. The
// token alone goes to out, so it can be captured; what it grants goes to info.
func runToken(args []string, out, info io.Writer) error {
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
	if _, err := fmt.Fprintln(out, tok); err != nil {
		return err
	}
	// Read the grant back as the auth server will: normalized, with its exp.
	c, err := token.Verify(tok, map[string]token.Key{key.ID: key.Public()}, time.Now())
	if err != nil {
		return fmt.Errorf("verify the signed token: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n  %-11s %s\n", "Publish:", grantLabel(c.Publish))
	fmt.Fprintf(&b, "  %-11s %s\n", "Subscribe:", grantLabel(c.Subscribe))
	fmt.Fprintf(&b, "  %-11s %s (in %s)\n", "Expires:", c.ExpiresAt.Format(time.DateTime), time.Until(c.ExpiresAt).Round(time.Second))
	fmt.Fprintf(&b, "  %-11s %s\n", "Key:", key.ID)
	fmt.Fprintf(&b, "  %-11s https://<relay>/?jwt=<token>\n", "Connect:")
	_, err = io.WriteString(info, b.String())
	return err
}

// grantLabel describes one role of a grant for the terminal.
func grantLabel(path string) string {
	if path == "" {
		return "-"
	}
	return path + "/**"
}
