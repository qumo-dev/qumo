package auth

import (
	"bufio"
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

On a terminal, both ask before writing a file or emitting a token; a
piped run confirms without asking, as does -yes.

Run "qumo auth <command> -h" for a command's flags.
`

// ErrDeclined says the user answered a confirmation prompt with no: the
// run took none of its effect.
var ErrDeclined = errors.New("qumo auth: declined")

// Run executes "qumo auth" with args, the arguments after "auth".
func Run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, cmdUsage)
		return errors.New("qumo auth: a command is needed")
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "keygen":
		return runKeygen(rest, os.Stdout, os.Stderr, prompted(os.Stdin, os.Stdout))
	case "token":
		return runToken(rest, os.Stdout, os.Stderr, prompted(os.Stdin, os.Stdout))
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
	return "a root at or beneath " + prefix + " or its HLS opt-in " + token.HLSDirectory + strings.TrimPrefix(prefix, "/")
}

// runKeygen writes a new signing key pair: the private key, for the app that
// signs tokens, and a key set holding its public key, for the relay. The key
// and the two paths it will be written to are shown before any file is; on a
// terminal it asks before writing them, and declines without having written
// one. The summary goes to out and the prompt to info.
func runKeygen(args []string, out, info io.Writer, in io.Reader) error {
	fs := flag.NewFlagSet("qumo auth keygen", flag.ContinueOnError)
	prefix := fs.String("prefix", "/", "confine the key to this directory (e.g. /acme/app/) and its HLS opt-in mirror (/.hls/acme/app/); / for none")
	privPath := fs.String("out", "signing-key.jwk", "where to write the private signing key (keep it on the app's server)")
	pubPath := fs.String("keys", "keys.json", "where to write the public key set (QUMO_AUTH_KEYS)")
	yes := fs.Bool("yes", false, "confirm without asking, as a piped run does")
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
	// The key and where it lies, before any file does: the key's prefix is
	// the one setting a re-issue won't give back, and a bare prefix makes
	// the key the tenant of everything.
	var fields strings.Builder
	fmt.Fprintf(&fields, "Generated an Ed25519 signing key.\n\n")
	fmt.Fprintf(&fields, "  %-13s %s\n", "Key ID:", key.ID)
	fmt.Fprintf(&fields, "  %-13s %s\n", "Grants:", prefixLabel(key.Prefix))
	fmt.Fprintf(&fields, "  %-13s %s  (private: keep it on your app's server)\n", "Signing key:", *privPath)
	fmt.Fprintf(&fields, "  %-13s %s  (public: for the relay)\n", "Key set:", *pubPath)
	var next strings.Builder
	fmt.Fprintf(&next, "\nNext:\n")
	fmt.Fprintf(&next, "  1. Run the relay with it: QUMO_AUTH_KEYS=%s qumo relay\n", *pubPath)
	fmt.Fprintf(&next, "  2. Sign a test token:      qumo auth token -key %s %s\n", *privPath, exampleGrant(key.Prefix))
	// On a terminal, the fields and the question stand before a file is
	// written: a tenant the prefix didn't mean, or a path that names the
	// wrong place, costs nothing to decline and re-run. A piped run, or
	// -yes, confirms without asking, and prints its summary only once it
	// took effect, as this command did before it asked.
	asks := in != nil && !*yes
	if asks {
		if _, err := io.WriteString(out, fields.String()); err != nil {
			return err
		}
	}
	if err := confirm(info, "Write these two files?", in, *yes, "nothing was written"); err != nil {
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
	if !asks {
		if _, err := io.WriteString(out, fields.String()); err != nil {
			return err
		}
	}
	_, err = io.WriteString(out, next.String())
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

// runToken signs a token with a signing key, for testing a relay by hand.
// The token alone goes to out, so it can be captured; what it grants, and
// the args the run will use, go to info. On a terminal it asks before the
// token is emitted, and declines having emitted nothing.
func runToken(args []string, out, info io.Writer, in io.Reader) error {
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
	yes := fs.Bool("yes", false, "confirm without asking, as a piped run does")
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
	fmt.Fprintf(&b, "  %-11s %s\n", "Present:", "https://<relay>/?jwt=<token>, or on a session's Auth Stream")
	if _, err := io.WriteString(info, b.String()); err != nil {
		return err
	}
	// On a terminal, the confirmation stands before the token is emitted: a
	// subject, root or pattern the args didn't mean to grant costs nothing
	// to decline and re-sign. A piped run, or -yes, confirms without asking,
	// so a run in a script or CI keeps stdout for the token alone.
	if err := confirm(info, "Sign and emit this token?", in, *yes, "no token was emitted"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, tok); err != nil {
		return err
	}
	// After the effect, the run's own example: the command that signs this
	// same grant again, with every arg the run takes — the defaults it
	// filled in, -key and -ttl, among them — spelled as a hand writes them.
	// Not part of what the confirm asks on: the shape above is what a
	// person answers with, and the example is a thing to copy once the
	// token exists.
	var eff []effArg
	eff = append(eff, effArg{name: "key", val: *keyPath})
	if c.Root != "" {
		eff = append(eff, effArg{name: "root", val: c.Root})
	}
	for _, a := range token.Actions() {
		if patterns := c.Patterns(a); len(patterns) > 0 {
			eff = append(eff, effArg{name: string(a), val: strings.Join(patterns, " ")})
		}
	}
	eff = append(eff,
		effArg{name: "sub", val: c.Subject},
		effArg{name: "ttl", val: spellDur(*ttl)},
	)
	_, err = fmt.Fprintf(info, "\n  Example:  qumo auth token %s\n", effArgs(eff))
	return err
}

// prompted returns in when both ends of the run sit on a console — stdin
// and stdout — where a person could answer, and nil otherwise: a run with
// either piped or redirected (the stdout of a capture — `T=$(qumo auth
// token …)` — and a script's or CI's input among them) confirms without
// asking.
func prompted(in, out *os.File) io.Reader {
	if !tty(in) || !tty(out) {
		return nil
	}
	return in
}

// tty reports whether f sits on a console a person could answer a prompt
// on.
func tty(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// effArg is one flag of the args a run will use: the flag, and the value
// the run takes it as.
type effArg struct {
	name, val string
}

// effArgs renders the args a run will use, on one line to check against
// what was expected: the flags a run takes, with the values it takes them
// as, whatever named them. An entry of an empty value names a flag the
// args left unset, and is left out — nothing of it to check.
func effArgs(args []effArg) string {
	var b strings.Builder
	for _, a := range args {
		if a.val == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("  ")
		}
		fmt.Fprintf(&b, "-%s %s", a.name, a.val)
	}
	return b.String()
}

// spellDur spells a duration as it is written by hand, "1h" and "5m",
// where Duration spells them "1h0m0s" and "5m0s"; one no hand writes so
// short a form of spells itself whole.
func spellDur(d time.Duration) string {
	s := d.String()
	switch {
	case strings.HasSuffix(s, "h0m0s"):
		return strings.TrimSuffix(s, "0m0s")
	case strings.HasSuffix(s, "m0s"):
		return strings.TrimSuffix(s, "0s")
	default:
		return s
	}
}

// confirm reads the answer to a y/N question from in: y or yes confirms,
// and a run takes its effect. The run that cannot be asked confirms
// without asking — in nil, first: a piped or redirected run; -yes, then:
// the answer named up front. Any other answer declines with decline, and
// the run takes none of its effect.
func confirm(info io.Writer, question string, in io.Reader, yes bool, decline string) error {
	if in == nil || yes {
		return nil
	}
	ok, err := ask(info, question, in)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrDeclined, decline)
	}
	return nil
}

// ask writes question to w and reads an answer from in: y or yes
// confirms, in any case. A bare Enter, a no, an answer it cannot read (an
// EOF among them) declines.
func ask(w io.Writer, question string, in io.Reader) (bool, error) {
	if _, err := fmt.Fprintf(w, "\n%s [y/N]: ", question); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		return false, nil // not actionable: an answer that can't be read declines
	}
	return strings.EqualFold(strings.TrimSpace(line), "y") ||
		strings.EqualFold(strings.TrimSpace(line), "yes"), nil
}
