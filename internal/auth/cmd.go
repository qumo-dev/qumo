package auth

import (
	"cmp"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qumo-dev/qumo/token"
)

const cmdUsage = `Usage: qumo auth <command>

Sets up the credentials a relay verifies (QUMO_AUTH_KEYS). An app signs a
capability token per client with its own Ed25519 key; the client connects
with it (?jwt=…) or presents it on a session's Auth Stream; the relay
verifies it against the key set and grants it until it expires.

Commands:
  keygen   Generate a signing key pair: the app's private key, and the public
           key set the relay trusts
  token    Sign a token by hand, for testing

Run "qumo auth <command> -h" for a command's flags.
`

// Run executes "qumo auth" with args, the arguments after "auth".
func Run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, cmdUsage)
		return errors.New("qumo auth: a command is needed")
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "keygen":
		return runKeygen(rest, os.Stdout)
	case "token":
		return runToken(rest, os.Stdout, os.Stderr)
	case "-h", "--help", "help":
		_, err := fmt.Fprint(os.Stdout, cmdUsage)
		return err
	default:
		fmt.Fprint(os.Stderr, cmdUsage)
		return fmt.Errorf("qumo auth: unknown command %q", cmd)
	}
}

// prefixLabel describes what a key with prefix may grant, for the terminal.
func prefixLabel(prefix string) string {
	if prefix == "/" {
		return "any root (no prefix)"
	}
	return "a root at or beneath " + prefix
}

// runKeygen writes a new signing key pair: the private key, for the app that
// signs tokens, and a key set holding its public key, for the relay.
func runKeygen(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("qumo auth keygen", flag.ContinueOnError)
	prefix := fs.String("prefix", "/", "confine the key to this directory (e.g. /acme/app/); / for none")
	privPath := fs.String("out", "signing-key.jwk", "where to write the private signing key (keep it on the app's server)")
	pubPath := fs.String("keys", "keys.json", "where to write the public key set (QUMO_AUTH_KEYS)")
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
	fmt.Fprintf(&b, "  %-13s %s  (public: for the relay)\n", "Key set:", *pubPath)
	fmt.Fprintf(&b, "\nNext:\n")
	fmt.Fprintf(&b, "  1. Run the relay with it: QUMO_AUTH_KEYS=%s qumo relay\n", *pubPath)
	fmt.Fprintf(&b, "  2. Sign a test token:      qumo auth token -key %s %s\n", *privPath, exampleGrant(key.Prefix))
	_, err = io.WriteString(out, b.String())
	return err
}

// exampleGrant is the flags of a grant a key with prefix may sign, for a
// usage example.
func exampleGrant(prefix string) string {
	return "-root " + prefix + " -publish /demo"
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
	root := fs.String("root", "", "the directory the patterns are relative to, at or beneath the key's prefix (e.g. /acme/room/42/); empty, with no patterns, grants nothing")
	var g token.Grant
	for _, f := range []struct {
		action   token.Action
		patterns *[]string
		usage    string
	}{
		{token.ActionPublish, &g.Publish, "publish through a relay"},
		{token.ActionSubscribe, &g.Subscribe, "receive live or as history"},
		{token.ActionCreate, &g.Create, "create tracks of at a funnel"},
		{token.ActionPost, &g.Post, "post into at a funnel"},
		{token.ActionRedact, &g.Redact, "redact records of at a funnel"},
	} {
		fs.Func(string(f.action), "a `PATTERN` relative to -root of the broadcasts the bearer may "+f.usage+
			" (repeatable): /x is that path, /x/ every path beneath it, / everything beneath the root; a segment may be a {name} wildcard",
			func(v string) error {
				*f.patterns = append(*f.patterns, v)
				return nil
			})
	}
	subject := fs.String("sub", "", "who the bearer is (the sub claim)")
	ttl := fs.Duration("ttl", time.Hour, "how long the token is valid (at most 1h)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	key, err := token.LoadSigningKey(*keyPath)
	if err != nil {
		return err
	}
	if *root != "" {
		if g.Root, err = token.Directory(*root); err != nil {
			return fmt.Errorf("-root: %w", err)
		}
	}
	g.Subject = *subject
	tok, err := token.Sign(key, g, *ttl)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, tok); err != nil {
		return err
	}
	// Read the grant back as the relay will: normalized, with its exp.
	c, err := token.Verify(tok, map[string]token.Key{key.ID: key.Public()}, time.Now())
	if err != nil {
		return fmt.Errorf("verify the signed token: %w", err)
	}
	var b strings.Builder
	b.WriteString("\n")
	fmt.Fprintf(&b, "  %-11s %s\n", "Root:", cmp.Or(c.Root, "-"))
	granted := false
	for _, a := range token.Actions() {
		if patterns := c.Patterns(a); len(patterns) > 0 {
			fmt.Fprintf(&b, "  %-11s %s\n", strings.ToUpper(string(a[:1]))+string(a[1:])+":", strings.Join(patterns, " "))
			granted = true
		}
	}
	if !granted {
		fmt.Fprintf(&b, "  %-11s nothing: the token names its key's tenant alone\n", "Grants:")
	}
	fmt.Fprintf(&b, "  %-11s %s\n", "Subject:", cmp.Or(c.Subject, "-"))
	fmt.Fprintf(&b, "  %-11s %s (in %s)\n", "Expires:", c.ExpiresAt.Format(time.DateTime), time.Until(c.ExpiresAt).Round(time.Second))
	fmt.Fprintf(&b, "  %-11s %s\n", "Key:", key.ID)
	fmt.Fprintf(&b, "  %-11s https://<relay>/?jwt=<token>, or on a session's Auth Stream\n", "Present:")
	_, err = io.WriteString(info, b.String())
	return err
}
