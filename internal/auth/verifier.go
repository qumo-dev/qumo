package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
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
// signed by a key in the set, have its root within the key's prefix, and be
// within its validity (token.Verify). Its grant lasts until it expires, as a
// ticket's does: the relay applies the expiry to the grant, not to the
// session, which a client keeps by presenting fresh tickets. A live session
// is re-checked every 30 s (token.VerifyLive): a key that has left the set
// ends its sessions. A key
// marked "pause": ["publish"] starts no new sessions that may publish (a
// credential with a publish pattern), and a funnel creates no track with it
// (Check); it
// never ends a live one, since it pauses what is new rather than what is on
// air. The key set is kept when a refresh fails (fail-static): after 6 h
// without one, new sessions are refused, while live ones continue on the
// last set.
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
// set, and ends when its key has left the set. The bytes of a revalidate are
// reported whatever it decides, since they were sent.
func (v *Verifier) Authorize(_ context.Context, req Request) (*Grant, error) {
	connect := req.Event == EventConnect
	credential, credErr := credentialOf(req.Query)
	g, s, err := v.decide(credential, credErr, connect, func(g *Grant) bool { return connect && publishesBroadcast(g.grant) })
	if v.usage != nil {
		// A session is learned only when it is admitted at connect. A
		// revalidate never adds one: it can finish after the session's end
		// was recorded, and must not bring it back.
		if err == nil && req.Event == EventConnect {
			v.usage.open(req.ID, s)
		}
		if req.Event == EventRevalidate {
			// A refused session is ending; one that couldn't be checked
			// lives on, and the relay asks again.
			_, refused := errors.AsType[RefusedError](err)
			v.usage.revalidated(req.ID, req.Bytes, !refused)
		}
	}
	return g, err
}

// Check verifies credential, presented with one request that needs action
// on the broadcast at path, such as a funnel's: as Authorize verifies a
// connect, except that the key's pause refuses only creating a track the
// credential may create, so a paused key's credentials still read, post and
// redact, and one that may not create is refused for that by the caller. It
// reports no usage.
func (v *Verifier) Check(credential string, action token.Action, path moqt.BroadcastPath) (*Grant, error) {
	g, _, err := v.decide(credential, nil, true, func(g *Grant) bool {
		return action == token.ActionCreate && g.Allows(token.ActionCreate, path)
	})
	return g, err
}

// Ticket verifies credential, a ticket presented on an Auth Stream of a
// session whose own credential keyID signed, as a connect is. The ticket must
// be signed by that same key, so that everything a session may do is its
// key's tenant's to grant and is reported under it. A key paused for
// publishing refuses one that may publish.
func (v *Verifier) Ticket(credential, keyID string) (*Grant, error) {
	g, _, err := v.decide(credential, nil, true, func(g *Grant) bool { return publishesBroadcast(g.grant) })
	if err != nil {
		return nil, err
	}
	if g.keyID != keyID {
		return nil, refuse(http.StatusForbidden, "the ticket is signed by key %s, not by the session's", g.keyID)
	}
	return g, nil
}

// Trusts reports whether g, a ticket's grant, is still in force as a live
// session's credential is: its key is in the current set, and its root
// within the key's prefix. It reads the set alone, so checking every ticket
// often costs no signature. An error is a set it can't judge by, such as
// none loaded yet: the ticket stays in force and is checked again.
func (v *Verifier) Trusts(g *Grant) (bool, error) {
	set, _ := v.store.current(v.now())
	if set == nil {
		return false, errors.New("auth: no key set loaded yet")
	}
	key, ok := set.keys[g.keyID]
	return ok && key.Holds(g.grant.Root), nil
}

// Publishing records that the session id was granted a ticket that may
// publish: the usage it reports is a publisher's from then on.
func (v *Verifier) Publishing(id string) {
	if v.usage != nil {
		v.usage.publishing(id)
	}
}

// credentialOf returns the credential a connect URL's raw query carries as
// jwt.
func credentialOf(rawQuery string) (string, error) {
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", refuse(http.StatusUnauthorized, "query: %v", err)
	}
	if !query.Has("jwt") {
		return "", refuse(http.StatusUnauthorized, "no credential: connect with ?jwt=")
	}
	return query.Get("jwt"), nil
}

// decide verifies credential, at connect or for a live session; credErr is
// why there is none, answered once the key set is known to be usable. A key
// paused for publishing refuses it when paused reports true for its grant.
func (v *Verifier) decide(credential string, credErr error, connect bool, paused func(*Grant) bool) (*Grant, usageSession, error) {
	now := v.now()
	set, fresh := v.store.current(now)
	switch {
	case set == nil:
		// Not a refusal: at connect the relay answers 503, and at revalidate
		// it retries while the session lives.
		return nil, usageSession{}, errors.New("auth: no key set loaded yet")
	case connect && !fresh:
		return nil, usageSession{}, fmt.Errorf("auth: the key set is older than %s", keysMaxStale)
	}

	if credErr != nil {
		return nil, usageSession{}, credErr
	}
	verify := token.VerifyLive
	if connect {
		verify = token.Verify
	}
	c, err := verify(credential, set.keys, now)
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, token.ErrForbidden) {
			status = http.StatusForbidden
		}
		return nil, usageSession{}, refuse(status, "%v", err)
	}
	g := &Grant{grant: c.Grant, revalidate: revalidateEvery, keyID: c.Key.ID, expires: c.ExpiresAt}
	if c.Key.Pauses(token.ActionPublish) && paused(g) {
		return nil, usageSession{}, refuse(http.StatusForbidden,
			"signing key %s starts nothing new that publishes", c.Key.ID)
	}
	return g, usageSession{kid: c.Key.ID, publishes: publishesBroadcast(c.Grant)}, nil
}

// publishesBroadcast reports whether g lets a relay session publish a
// broadcast: a publish pattern, which announces it. A relay ignores create,
// which a funnel checks itself (Check), and a bearer that may only post is a
// viewer, such as a chat sender posting into a funnel's track.
func publishesBroadcast(g token.Grant) bool {
	return len(g.Publish) > 0
}

// End reports a session's end with its final byte totals.
func (v *Verifier) End(_ context.Context, req Request) error {
	if v.usage == nil {
		return nil
	}
	v.usage.close(req.ID, req.Bytes, req.Reason)
	return nil
}
