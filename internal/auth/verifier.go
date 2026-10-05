package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/qumo-dev/qumo/token"
)

// revalidateEvery is how often the relay re-checks a live session against
// the current key set: how soon a withdrawn key ends its sessions.
const revalidateEvery = 30 * time.Second

// VerifierConfig configures a relay that verifies credentials itself.
type VerifierConfig struct {
	// Keys is where the key set is (QUMO_AUTH_KEYS): an https URL (or http
	// on a loopback host) to download about every 30 s, or a file, as a path
	// or a file:// URL, re-read when it changes.
	Keys string
	// UsageURL receives the sessions' usage records (QUMO_USAGE_URL);
	// empty reports nothing.
	UsageURL string
	// Token is sent as a bearer token to a key-set URL and UsageURL
	// (QUMO_RELAY_TOKEN).
	Token string
}

// Verifier is the relay's session auth when it verifies credentials itself,
// against a key set, rather than asking an auth server. Its Authorize and End
// take the place of the Client's.
//
// A session's credential (the jwt query parameter of its connect URL) must be
// signed by a key in the set and grant only paths within the key's prefix
// (token.Verify). A live session is re-checked every 30 s: a key that has left
// the set ends its sessions, while a key marked "admit": false keeps them and
// starts no new ones. The key set is kept when a refresh fails (fail-static);
// after 6 h without one, new sessions are refused.
type Verifier struct {
	source keySource
	store  keyStore
	usage  *usageReporter
	now    func() time.Time
}

// NewVerifier returns a Verifier for cfg. Run starts its key-set refresh
// and usage reporting.
func NewVerifier(cfg VerifierConfig) (*Verifier, error) {
	src, err := keySourceFor(cfg.Keys, cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("QUMO_AUTH_KEYS: %w", err)
	}
	v := &Verifier{source: src, now: time.Now}
	if cfg.UsageURL != "" {
		u, err := newUsageReporter(cfg.UsageURL, cfg.Token)
		if err != nil {
			return nil, fmt.Errorf("QUMO_USAGE_URL: %w", err)
		}
		v.usage = u
	}
	return v, nil
}

// Source names the key set's source, for the startup banner.
func (v *Verifier) Source() string { return v.source.String() }

// Reporting reports whether the Verifier sends usage.
func (v *Verifier) Reporting() bool { return v.usage != nil }

// Run refreshes the key set and sends usage until ctx ends. The usage
// reporter sends what is left once more after that, so ctx should end after
// the relay has stopped taking sessions.
func (v *Verifier) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { runKeySource(ctx, v.source, &v.store, v.now) })
	if v.usage != nil {
		wg.Go(func() { v.usage.run(ctx) })
	}
	wg.Wait()
}

// refusal is a RefusedError with the reason, for the relay's log.
type refusal struct {
	RefusedError
	reason string
}

func (r refusal) Error() string { return fmt.Sprintf("%s: %s", r.RefusedError.Error(), r.reason) }

// Unwrap lets errors.AsType find the RefusedError, whose status the relay
// answers with.
func (r refusal) Unwrap() error { return r.RefusedError }

func refuse(status int, format string, args ...any) error {
	return refusal{RefusedError: RefusedError{Status: status}, reason: fmt.Sprintf(format, args...)}
}

// Authorize verifies a connect or revalidate. A new session needs a key set
// no older than the fail-static limit, and a key that admits; a live one
// keeps going on a stale set or a non-admitting key, and ends when its key
// has left the set. The bytes of a revalidate are reported whatever it
// decides, since they were sent.
func (v *Verifier) Authorize(_ context.Context, req Request) (*Grant, error) {
	g, s, err := v.decide(req)
	if v.usage != nil {
		// A session is learned only when it is admitted at connect. A
		// revalidate never adds one: it can finish after the session's end
		// was recorded, and must not bring it back.
		if err == nil && req.Event == EventConnect {
			v.usage.open(req.ID, s)
		}
		if req.Event == EventRevalidate && (req.Bytes.Sent > 0 || req.Bytes.Received > 0) {
			v.usage.reportUsage(req.ID, req.Bytes)
		}
	}
	return g, err
}

func (v *Verifier) decide(req Request) (*Grant, usageSession, error) {
	now := v.now()
	connect := req.Event == EventConnect
	set, fresh := v.store.current(now)
	switch {
	case set == nil:
		// Not a refusal: at connect the relay answers 503, and at revalidate
		// it retries while the session lives.
		return nil, usageSession{}, errors.New("auth: no key set loaded yet")
	case connect && !fresh:
		return nil, usageSession{}, fmt.Errorf("auth: the key set is older than %s", keysMaxStale)
	}

	query, err := url.ParseQuery(req.Query)
	if err != nil {
		return nil, usageSession{}, refuse(http.StatusUnauthorized, "query: %v", err)
	}
	if !query.Has("jwt") {
		return nil, usageSession{}, refuse(http.StatusUnauthorized, "no credential: connect with ?jwt=")
	}
	c, err := token.Verify(query.Get("jwt"), set.keys, now)
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, token.ErrForbidden) {
			status = http.StatusForbidden
		}
		return nil, usageSession{}, refuse(status, "%v", err)
	}
	if connect && set.noAdmit[c.Key.ID] {
		return nil, usageSession{}, refuse(http.StatusForbidden, "signing key %s starts no new sessions", c.Key.ID)
	}

	expires := c.ExpiresAt.Add(token.Leeway)
	g := &Grant{expires: expires, revalidate: revalidateEvery}
	s := usageSession{kid: c.Key.ID, jti: c.ID, expires: expires}
	if c.Publish != "" {
		g.Publish = Patterns{{base: c.Publish}}
		s.role = rolePublish
	}
	if c.Subscribe != "" {
		g.Subscribe = Patterns{{base: c.Subscribe}}
		s.role = roleSubscribe
	}
	if c.Publish != "" && c.Subscribe != "" {
		s.role = roleBoth
	}
	return g, s, nil
}

// End reports a session's end with its final byte totals.
func (v *Verifier) End(_ context.Context, req Request) error {
	if v.usage == nil {
		return nil
	}
	v.usage.close(req.ID, req.Bytes, req.Reason)
	return nil
}
