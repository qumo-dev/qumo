package auth

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/token"
)

// The whole developer flow: keygen writes the two files, token signs with the
// private one, and a relay loading the public one admits the token.
func TestRunKeygen_TokenAndVerifierAgree(t *testing.T) {
	dir := t.TempDir()
	priv, pub := filepath.Join(dir, "signing-key.jwk"), filepath.Join(dir, "keys.json")
	var out bytes.Buffer

	require.NoError(t, runKeygen([]string{"-prefix", "acme/app", "-out", priv, "-keys", pub}, &out, io.Discard, nil))
	assert.Contains(t, out.String(), "Key ID:")
	out.Reset()
	var info bytes.Buffer
	require.NoError(t, runToken([]string{"-key", priv, "-root", "/acme/app/",
		"-publish", "/alice", "-publish", "/alice/", "-subscribe", "/", "-ttl", "5m"}, &out, &info, nil))
	tok := strings.TrimSpace(out.String())
	assert.Equal(t, 2, strings.Count(tok, "."), "stdout carries the token alone, so it can be captured")
	assert.Contains(t, info.String(), "Root:       /acme/app/\n", "the summary shows the grant as the relay will")
	assert.Contains(t, info.String(), "Publish:    /alice /alice/\n")
	v, err := NewVerifier(VerifierConfig{Keys: pub})
	require.NoError(t, err)

	g, err := v.Authorize(t.Context(), Request{ID: "s1", Event: EventConnect, Query: "jwt=" + tok})

	require.NoError(t, err)
	assert.True(t, g.Allows(token.ActionPublish, moqt.BroadcastPath("/acme/app/alice/cam")))
	assert.True(t, g.Allows(token.ActionPublish, moqt.BroadcastPath("/acme/app/alice")))
	assert.False(t, g.Allows(token.ActionPublish, moqt.BroadcastPath("/acme/app/bob")))
	assert.True(t, g.Allows(token.ActionSubscribe, moqt.BroadcastPath("/acme/app/bob")))
	assert.Equal(t, moqt.Grant{Root: "/acme/app/", Publish: []string{"/alice", "/alice/"}, Subscribe: []string{"/"}}, g.Token().Grant)
}

func TestRun_NoCommand(t *testing.T) {
	err := Run(nil)

	assert.ErrorContains(t, err, "a command is needed")
}

func TestRunKeygen_RefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	args := []string{"-out", filepath.Join(dir, "signing-key.jwk"), "-keys", filepath.Join(dir, "keys.json")}
	require.NoError(t, runKeygen(args, &bytes.Buffer{}, io.Discard, nil))

	err := runKeygen(args, &bytes.Buffer{}, io.Discard, nil)

	assert.Error(t, err, "a second keygen must not replace the key every token was signed with")
}

// A keygen that can't write the key set leaves no private key behind, so a
// retry isn't refused by a key whose public half was never saved.
func TestRunKeygen_KeySetFailureRemovesThePrivateKey(t *testing.T) {
	dir := t.TempDir()
	priv, pub := filepath.Join(dir, "signing-key.jwk"), filepath.Join(dir, "keys.json")
	require.NoError(t, os.WriteFile(pub, []byte("{}"), 0o600))

	err := runKeygen([]string{"-out", priv, "-keys", pub}, &bytes.Buffer{}, io.Discard, nil)

	require.Error(t, err)
	assert.NoFileExists(t, priv)
}

func TestRun_UnknownCommand(t *testing.T) {
	err := Run([]string{"nope"})

	assert.ErrorContains(t, err, "unknown command")
}

// Each action's flag adds a pattern relative to -root, and may be repeated.
func TestRunToken_Patterns(t *testing.T) {
	dir := t.TempDir()
	priv, pub := filepath.Join(dir, "signing-key.jwk"), filepath.Join(dir, "keys.json")
	require.NoError(t, runKeygen([]string{"-prefix", "acme/app", "-out", priv, "-keys", pub}, &bytes.Buffer{}, io.Discard, nil))
	var out, info bytes.Buffer

	require.NoError(t, runToken([]string{"-key", priv, "-sub", "42", "-root", "acme/app/room/1",
		"-publish", "/7", "-subscribe", "/", "-create", "/comments",
		"-post", "/comments", "-post", "/7/", "-post", "/8", "-redact", "/{user}/comments"}, &out, &info, nil))

	assert.Contains(t, info.String(), "Root:       /acme/app/room/1/\n", "a bare -root reads as the directory it names")
	assert.Contains(t, info.String(), "Publish:    /7\n")
	assert.Contains(t, info.String(), "Subscribe:  /\n")
	assert.Contains(t, info.String(), "Create:     /comments\n")
	assert.Contains(t, info.String(), "Post:       /comments /7/ /8\n")
	assert.Contains(t, info.String(), "Redact:     /{user}/comments\n")
	assert.Contains(t, info.String(), "Subject:    42\n")
	v, err := NewVerifier(VerifierConfig{Keys: pub})
	require.NoError(t, err)
	g, err := v.Authorize(t.Context(), Request{ID: "s1", Event: EventConnect, Query: "jwt=" + strings.TrimSpace(out.String())})
	require.NoError(t, err)
	assert.Equal(t, "42", g.Subject())
	tests := map[string]struct {
		action token.Action
		path   moqt.BroadcastPath
		want   bool
	}{
		"publish the exact path":       {action: token.ActionPublish, path: "/acme/app/room/1/7", want: true},
		"publish beneath it":           {action: token.ActionPublish, path: "/acme/app/room/1/7/x"},
		"subscribe beneath the root":   {action: token.ActionSubscribe, path: "/acme/app/room/1/9", want: true},
		"create":                       {action: token.ActionCreate, path: "/acme/app/room/1/comments", want: true},
		"post":                         {action: token.ActionPost, path: "/acme/app/room/1/comments", want: true},
		"post beneath a directory":     {action: token.ActionPost, path: "/acme/app/room/1/7/x", want: true},
		"post, a third pattern":        {action: token.ActionPost, path: "/acme/app/room/1/8", want: true},
		"post the directory itself":    {action: token.ActionPost, path: "/acme/app/room/1/7"},
		"redact, a wildcard":           {action: token.ActionRedact, path: "/acme/app/room/1/bob/comments", want: true},
		"redact, the wildcard missing": {action: token.ActionRedact, path: "/acme/app/room/1/comments"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, g.Allows(tt.action, tt.path))
		})
	}
}

func TestRunToken_Refusals(t *testing.T) {
	tests := map[string]struct {
		args    []string
		wantErr string
	}{
		"a root outside the key's prefix": {args: []string{"-root", "other", "-publish", "/"}, wantErr: "outside"},
		"a pattern with no slash":         {args: []string{"-root", "acme/app", "-subscribe", "live"}, wantErr: "subscribe"},
		"a malformed wildcard":            {args: []string{"-root", "acme/app", "-post", "/{x"}, wantErr: "post"},
		"the scope flag is gone":          {args: []string{"-scope", "publish:acme/app"}, wantErr: "-scope"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			priv := filepath.Join(dir, "signing-key.jwk")
			require.NoError(t, runKeygen([]string{"-prefix", "acme/app", "-out", priv, "-keys", filepath.Join(dir, "keys.json")}, &bytes.Buffer{}, io.Discard, nil))
			var out bytes.Buffer

			err := runToken(append([]string{"-key", priv}, tt.args...), &out, &bytes.Buffer{}, nil)

			assert.ErrorContains(t, err, tt.wantErr)
			assert.Empty(t, out.String(), "no token is printed")
		})
	}
}

// A token of no patterns says so in the summary.
func TestRunToken_NoPatterns(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "signing-key.jwk")
	require.NoError(t, runKeygen([]string{"-prefix", "acme", "-out", priv, "-keys", filepath.Join(dir, "keys.json")}, &bytes.Buffer{}, io.Discard, nil))
	var out, info bytes.Buffer

	require.NoError(t, runToken([]string{"-key", priv}, &out, &info, nil))

	assert.Equal(t, 2, strings.Count(strings.TrimSpace(out.String()), "."))
	assert.Contains(t, info.String(), "nothing: the token names its key's tenant alone")
}

// The ticket flag is gone: any token is presented at connect or on a
// session's Auth Stream alike.
func TestRunToken_NoTicketFlag(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "signing-key.jwk")
	require.NoError(t, runKeygen([]string{"-prefix", "acme", "-out", priv, "-keys", filepath.Join(dir, "keys.json")}, &bytes.Buffer{}, io.Discard, nil))
	var out, info bytes.Buffer

	assert.ErrorContains(t, runToken([]string{"-key", priv, "-ticket"}, &out, &info, nil), "-ticket")
	require.NoError(t, runToken([]string{"-key", priv, "-root", "/acme/room/1/", "-subscribe", "/"}, &out, &info, nil))
	assert.Contains(t, info.String(), "?jwt=<token>, or on a session's Auth Stream")
}

func TestRunKeygen_SaysWhatTheKeyGrants(t *testing.T) {
	tests := map[string]struct {
		prefix string
		want   []string
	}{
		"a prefix": {prefix: "acme/app", want: []string{"a root at or beneath /acme/app/ or its HLS opt-in /.hls/acme/app/", "-root /acme/app/ -publish /demo"}},
		"none":     {want: []string{"any root (no prefix)", "-root / -publish /demo"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			var out bytes.Buffer

			require.NoError(t, runKeygen([]string{"-prefix", tt.prefix,
				"-out", filepath.Join(dir, "signing-key.jwk"), "-keys", filepath.Join(dir, "keys.json")}, &out, io.Discard, nil))

			for _, w := range tt.want {
				assert.Contains(t, out.String(), w)
			}
		})
	}
}

// The summary names the two paths and what the key grants, and stops
// there: every value a run will use is in it already, so it adds no
// args line of its own.
func TestRunKeygen_NoEffectiveArgs(t *testing.T) {
	dir := t.TempDir()
	priv, pub := filepath.Join(dir, "signing-key.jwk"), filepath.Join(dir, "keys.json")
	var out bytes.Buffer

	require.NoError(t, runKeygen([]string{"-out", priv, "-keys", pub}, &out, io.Discard, nil))

	assert.Contains(t, out.String(), "Signing key:  "+priv+"  (private:")
	assert.Contains(t, out.String(), "Key set:      "+pub+"  (public:")
	assert.NotContains(t, out.String(), "Effective:")
}

// confirmCases are the answers a confirmation takes, and whether each
// confirms: y and its spellings do; a bare Enter, a no, and an answer
// that cannot be read (an EOF among them) decline.
type confirmCase struct {
	name    string
	answer  string
	wantErr bool
}

func confirmCases() []confirmCase {
	return []confirmCase{
		{name: "y", answer: "y\n"},
		{name: "yes", answer: "Yes\n"},
		{name: "n", answer: "n\n", wantErr: true},
		{name: "a bare Enter", answer: "\n", wantErr: true},
		{name: "an EOF", answer: "", wantErr: true},
	}
}

// On a terminal, keygen asks before it writes: y and yes write both
// files, any other answer — an interrupted prompt among them — writes
// none.
func TestRunKeygen_Confirm(t *testing.T) {
	for _, tt := range confirmCases() {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			priv, pub := filepath.Join(dir, "signing-key.jwk"), filepath.Join(dir, "keys.json")
			var out, info bytes.Buffer

			err := runKeygen([]string{"-out", priv, "-keys", pub}, &out, &info, strings.NewReader(tt.answer))

			assert.Contains(t, out.String(), "Signing key:  "+priv, "the key and its paths are shown before the answer is read")
			if tt.wantErr {
				assert.ErrorIs(t, err, ErrDeclined)
				assert.Contains(t, err.Error(), "nothing was written")
				assert.NoFileExists(t, priv)
				assert.NoFileExists(t, pub)
				assert.Contains(t, info.String(), "Write these two files?")
				return
			}
			require.NoError(t, err)
			assert.FileExists(t, priv)
			assert.FileExists(t, pub)
			assert.Contains(t, out.String(), "Next:")
		})
	}
}

// -yes answers the prompt up front, wherever it is directed at.
func TestRunKeygen_YesSkipsThePrompt(t *testing.T) {
	dir := t.TempDir()
	priv, pub := filepath.Join(dir, "signing-key.jwk"), filepath.Join(dir, "keys.json")
	var out, info bytes.Buffer

	require.NoError(t, runKeygen([]string{"-out", priv, "-keys", pub, "-yes"}, &out, &info, strings.NewReader("n\n")))

	assert.FileExists(t, priv)
	assert.Empty(t, info.String(), "a confirmed run asks nothing")
}

// The effective args a run will use are shown beside the shape they
// produce.
func TestRunToken_EffectiveArgs(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "signing-key.jwk")
	require.NoError(t, runKeygen([]string{"-prefix", "acme/app", "-out", priv, "-keys", filepath.Join(dir, "keys.json")}, &bytes.Buffer{}, io.Discard, nil))
	tests := map[string]struct {
		args []string
		want string
	}{
		"what the args named and the defaults it filled in": {
			args: []string{"-root", "acme/app/room/1", "-sub", "alice", "-post", "/comments"},
			want: "-key " + priv + "  -root /acme/app/room/1/  -post /comments  -sub alice  -ttl 1h",
		},
		"a repeated pattern flag lists its patterns": {
			args: []string{"-root", "/acme/app/", "-publish", "/alice", "-publish", "/alice/"},
			want: "-key " + priv + "  -root /acme/app/  -publish /alice /alice/  -ttl 1h",
		},
		"no patterns at all: what it grants and what it defaults alone": {
			args: nil,
			want: "-key " + priv + "  -ttl 1h",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var out, info bytes.Buffer

			require.NoError(t, runToken(append([]string{"-key", priv}, tt.args...), &out, &info, nil))

			assert.Contains(t, info.String(), "Effective:  "+tt.want+"\n")
		})
	}
}

// On a terminal, token asks before it emits: y and yes emit the token
// alone on stdout, any other answer — an interrupted prompt among them —
// emits nothing.
func TestRunToken_Confirm(t *testing.T) {
	for _, tt := range confirmCases() {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			priv := filepath.Join(dir, "signing-key.jwk")
			require.NoError(t, runKeygen([]string{"-prefix", "acme/app", "-out", priv, "-keys", filepath.Join(dir, "keys.json")}, &bytes.Buffer{}, io.Discard, nil))
			var out, info bytes.Buffer

			err := runToken([]string{"-key", priv, "-root", "/acme/app/", "-subscribe", "/"}, &out, &info, strings.NewReader(tt.answer))

			assert.Contains(t, info.String(), "Effective:", "the args are shown before the answer is read")
			if tt.wantErr {
				assert.ErrorIs(t, err, ErrDeclined)
				assert.Contains(t, err.Error(), "no token was emitted")
				assert.Empty(t, out.String(), "no token is emitted")
				assert.Contains(t, info.String(), "Sign and emit this token?")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 2, strings.Count(strings.TrimSpace(out.String()), "."))
		})
	}
}

// -yes answers the prompt up front, so a run a person copied from a
// script never waits on one.
func TestRunToken_YesSkipsThePrompt(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "signing-key.jwk")
	require.NoError(t, runKeygen([]string{"-prefix", "acme/app", "-out", priv, "-keys", filepath.Join(dir, "keys.json")}, &bytes.Buffer{}, io.Discard, nil))
	var out, info bytes.Buffer

	require.NoError(t, runToken([]string{"-key", priv, "-root", "/acme/app/", "-subscribe", "/", "-yes"},
		&out, &info, strings.NewReader("n\n")))

	assert.Equal(t, 2, strings.Count(strings.TrimSpace(out.String()), "."))
	assert.NotContains(t, info.String(), "Sign and emit this token?", "a confirmed run asks nothing")
}

// ask's answers: y and yes confirm in their case, while a bare Enter, a
// no and an unread answer decline.
func TestAsk(t *testing.T) {
	tests := map[string]struct {
		in   interface{ Read([]byte) (int, error) }
		want bool
	}{
		"y":             {in: strings.NewReader("y\n"), want: true},
		"yes":           {in: strings.NewReader("Yes\n"), want: true},
		"n":             {in: strings.NewReader("n\n"), want: false},
		"a bare Enter":  {in: strings.NewReader("\n"), want: false},
		"an EOF":        {in: strings.NewReader(""), want: false},
		"an unread one": {in: fakeErrReader{err: io.ErrClosedPipe}, want: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var asked bytes.Buffer

			ok, err := ask(&asked, "Anything?", tt.in)

			require.NoError(t, err)
			assert.Equal(t, tt.want, ok)
			assert.Contains(t, asked.String(), "Anything? [y/N]: ")
		})
	}
}

// A run with either end off a console — a capture's piped stdout, a
// script's redirected input among them — confirms without asking.
func TestPrompted(t *testing.T) {
	dir := t.TempDir()
	redirected, err := os.Create(filepath.Join(dir, "redirected"))
	require.NoError(t, err)
	defer func() { _ = redirected.Close() }() // not actionable: a read-only handle for the Stat

	assert.Nil(t, prompted(os.Stdin, redirected), "a captured stdout confirms without asking")
	assert.Nil(t, prompted(redirected, os.Stdout), "a redirected stdin confirms without asking")
}
