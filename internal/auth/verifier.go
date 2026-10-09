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
	// KeysCache is a file a downloaded key set is kept in between runs
	// (QUMO_AUTH_KEYS_CACHE), so a relay that restarts while the URL can't
	// be reached still has its keys. Empty keeps none. For a URL only.
	KeysCache string
	// UsageURL receives the sessions' usage records (QUMO_USAGE_URL);
	// empty reports nothing.
	UsageURL string
	// Token is sent as a bearer token to a key-set URL and UsageURL
	// (QUMO_RELAY_TOKEN).
	Token string
}

// Verifier is the relay's session auth: it verifies credentials against a
// key set. Its Authorize and End are what the relay's Server takes.
//
// A session's credential (the jwt query parameter of its connect URL) must be
// signed by a key in the set, grant only paths within the key's prefix, and be
// within its validity (token.Verify). A live session is re-checked every 30 s
// (token.VerifyLive): a key that has left the set ends its sessions. The
// credential's expiry decides only whether the session may start. A key
// marked "publish": false starts no new sessions that may publish (a
// credential with a scope permitting publish); it never ends a live one,
// since it pauses what is new rather than what is on air. The key set is kept
// when a refresh fails (fail-static): after 6 h without one, new sessions are
// refused, while live ones continue on the last set.
type Verifier struct {
	source keySource
	store  keyStore
	usage  *usageReporter
	now    func() time.Time
}

// NewVerifier returns a Verifier for cfg, with the key set a file or a cache
// already holds loaded. A key set file that can't be read is an error: the
// relay would run and refuse every session. Run starts the key-set refresh
// and usage reporting.
func NewVerifier(cfg VerifierConfig) (*Verifier, error) {
	src, err := keySourceFor(cfg.Keys, cfg.Token, cfg.KeysCache)
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
	// Last, once the rest of the configuration is known to be good.
	if err := src.start(&v.store, v.now()); err != nil {
		return nil, fmt.Errorf("QUMO_AUTH_KEYS: %w", err)
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
// no older than the fail-static limit; a live one keeps going on a stale
// set, and ends when its key has left the set. The bytes of a revalidate are reported whatever it
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
		if req.Event == EventRevalidate {
			v.usage.revalidated(req.ID, req.Bytes)
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
	verify := token.VerifyLive
	if connect {
		verify = token.Verify
	}
	c, err := verify(query.Get("jwt"), set.keys, now)
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, token.ErrForbidden) {
			status = http.StatusForbidden
		}
		return nil, usageSession{}, refuse(status, "%v", err)
	}
	publishes, subscribes := rolesOf(c.Grant)
	if connect && publishes && set.noPublish[c.Key.ID] {
		return nil, usageSession{}, refuse(http.StatusForbidden,
			"signing key %s starts no new publishing sessions", c.Key.ID)
	}

	g := &Grant{scopes: c.Scopes, subject: c.Subject, revalidate: revalidateEvery}
	s := usageSession{kid: c.Key.ID}
	switch {
	case publishes && subscribes:
		s.role = roleBoth
	case publishes:
		s.role = rolePublish
	case subscribes:
		s.role = roleSubscribe
	}
	return g, s, nil
}

// rolesOf reports whether g lets its bearer publish (a scope permitting
// publish) and subscribe (a scope permitting subscribe or fetch).
func rolesOf(g token.Grant) (publishes, subscribes bool) {
	for _, s := range g.Scopes {
		for _, a := range s.Actions {
			switch a {
			case token.ActionPublish:
				publishes = true
			case token.ActionSubscribe, token.ActionFetch:
				subscribes = true
			}
		}
	}
	return publishes, subscribes
}

// End reports a session's end with its final byte totals.
func (v *Verifier) End(_ context.Context, req Request) error {
	if v.usage == nil {
		return nil
	}
	v.usage.close(req.ID, req.Bytes, req.Reason)
	return nil
}
