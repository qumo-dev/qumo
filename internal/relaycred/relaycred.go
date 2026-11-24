// Package relaycred keeps a relay session's grant in force for a client that
// signs its own credentials, such as the funnel and the HLS egress. A relay
// grants a credential until its token expires, so a session that lives
// longer presents a fresh one on its Auth Stream before then.
package relaycred

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/qumo-dev/qumo/token"
)

const (
	// renewAt is the share of a grant's lifetime after which a fresh
	// credential is presented.
	renewAt = 2.0 / 3
	// retryInterval is the wait before presenting a credential again after
	// one could not be signed or was not granted.
	retryInterval = 10 * time.Second
	// presentTimeout bounds the wait for the relay's answer to a credential.
	presentTimeout = 10 * time.Second
)

// Sign signs a credential granting g with key, for token.MaxLifetime, and
// returns when its grant lapses at the latest: the relay grants it until its
// exp.
func Sign(key token.SigningKey, g token.Grant) (string, time.Time, error) {
	lapses := time.Now().Add(token.MaxLifetime)
	tok, err := token.Sign(key, g, token.MaxLifetime)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign relay credential: %w", err)
	}
	return tok, lapses, nil
}

// Target returns relayURL carrying a credential granting g, freshly signed
// with key, as ?jwt= in place of any it carried, and when that credential's
// grant lapses (Sign).
func Target(relayURL string, key token.SigningKey, g token.Grant) (string, time.Time, error) {
	credential, lapses, err := Sign(key, g)
	if err != nil {
		return "", time.Time{}, err
	}
	u, err := url.Parse(relayURL)
	if err != nil {
		// url.Parse's error quotes the URL, whose query may carry a
		// credential: report only that it is invalid.
		return "", time.Time{}, errors.New("RELAY_URL: invalid URL")
	}
	query := u.Query()
	query.Set("jwt", credential)
	u.RawQuery = query.Encode()
	return u.String(), lapses, nil
}

// RelayName describes relayURL for a startup banner without its credential:
// "none" for no relay.
func RelayName(relayURL string) string {
	if relayURL == "" {
		return "none"
	}
	u, err := url.Parse(relayURL)
	if err != nil {
		return "invalid"
	}
	return u.Scheme + "://" + u.Host + u.Path
}

// Dial opens a session to relayURL using dialer and mux. When key is non-nil it
// signs a fresh credential granting grant in place of any ?jwt= in relayURL
// and starts [Keep] to renew it on the session before its grant lapses.
func Dial(ctx context.Context, dialer *moqt.Dialer, relayURL string, mux *moqt.TrackMux, key *token.SigningKey, grant token.Grant) (*moqt.Session, error) {
	target := relayURL
	var lapses time.Time
	if key != nil {
		var err error
		target, lapses, err = Target(relayURL, *key, grant)
		if err != nil {
			return nil, err
		}
	}
	sess, err := dialer.Dial(ctx, target, mux)
	if err != nil {
		return nil, err
	}
	if key != nil {
		go Keep(sess, *key, grant, lapses)
	}
	return sess, nil
}

// credential is a token presented on an Auth Stream, as keeping a grant in
// force needs one: *moqt.Credential.
type credential interface {
	Grant() moqt.Grant
	Close() error
}

// Keep keeps sess's grant in force until sess ends: it presents a credential
// granting g, signed with key, on sess's Auth Stream each time the grant in
// force is two thirds through its lifetime. The first grant is the one sess
// connected with, which lapses at lapses (Sign returned it). A credential that
// can't be signed or is not granted is tried again every retryInterval; when
// the grant in force would lapse first, sess is closed, so that its owner
// dials again with a fresh credential rather than hold a session that may do
// nothing. A relay that verifies no credentials (NOT_SUPPORTED) lets the
// session's grant last for good, and Keep stops.
func Keep(sess *moqt.Session, key token.SigningKey, g token.Grant, lapses time.Time) {
	keep(sess.Context(), slog.Default(), renewGrant, presentOn(sess), func() {
		// not actionable: the session is ending either way.
		_ = sess.CloseWithError(moqt.UnauthorizedSessionErrorCode, "relay credential lapsed")
	}, func() (string, time.Time, error) { return Sign(key, g) }, lapses)
}

// Hold adds g to sess's grant until sess ends: it presents a credential
// granting g, signed with key, on sess's Auth Stream as a ticket, and a fresh
// one each time the ticket in force is two thirds through its lifetime, as
// Keep does. A ticket that can't be signed or is not granted is tried again
// every retryInterval, and sess is kept: what it connected with stays in
// force, so a relay that grants g later (a key set that comes to hold an
// opt-in) takes the ticket then. A relay that verifies no credentials
// (NOT_SUPPORTED) needs no ticket, and Hold stops.
func Hold(sess *moqt.Session, key token.SigningKey, g token.Grant) {
	// No grant in force yet (a zero lapses): the first ticket is presented
	// at once.
	keep(sess.Context(), slog.Default(), renewTicket, presentOn(sess), nil, func() (string, time.Time, error) { return Sign(key, g) }, time.Time{})
}

// presentOn presents a credential on sess's Auth Stream. It returns a nil
// credential with an error: a nil *moqt.Credential returned as is would be a
// credential interface that is not nil.
func presentOn(sess *moqt.Session) func(context.Context, []byte) (credential, error) {
	return func(ctx context.Context, tok []byte) (credential, error) {
		c, err := sess.Auth(ctx, tok)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}

// renewal is what keep keeps in force, which sets what it does with a
// credential not granted.
type renewal int

const (
	// renewGrant renews the grant a session connected with (Keep): a
	// credential not granted is retried, warning each time, until the
	// grant in force would lapse first; then keep ends.
	renewGrant renewal = iota
	// renewTicket renews a ticket added to a session's grant (Hold): a
	// credential not granted is retried for as long as the session lasts,
	// warning once, and noting when one is granted again.
	renewTicket
)

// keep is Keep and Hold, as r says: it presents credentials signed with sign
// through present, logging to log, until ctx ends. The first is presented two
// thirds of the way to lapses. end, when not nil, is called when keep ends
// because the grant lapses (renewGrant only); a nil end does nothing then.
func keep(ctx context.Context, log *slog.Logger, r renewal, present func(context.Context, []byte) (credential, error), end func(),
	sign func() (string, time.Time, error), lapses time.Time) {
	var held credential
	defer func() {
		if held != nil {
			_ = held.Close() // not actionable: the session is ending
		}
	}()
	wait := time.Duration(float64(time.Until(lapses)) * renewAt)
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		next, err := renew(ctx, present, sign)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if notSupported(err) {
				log.Info("relay credential not renewed: the relay verifies none, and the session's grant does not lapse")
				return
			}
			if r == renewTicket {
				// A ticket refused stays refused until the relay's key set
				// changes: say so once, not every retry.
				if !failing {
					log.Warn("relay ticket not granted, retrying", "error", err)
				}
				failing = true
				wait = retryInterval
				continue
			}
			// A retry signs, then may wait presentTimeout for an answer: the
			// session must not outlive its grant meanwhile.
			if time.Until(lapses) <= retryInterval+presentTimeout {
				log.Error("relay credential lapsing: closing the session to dial again", "error", err)
				if end != nil {
					end()
				}
				return
			}
			log.Warn("relay credential not renewed, retrying", "error", err, "lapses_in", time.Until(lapses).Round(time.Second))
			wait = retryInterval
			continue
		}
		if failing {
			log.Info("relay ticket granted", "root", next.Grant().Root)
			failing = false
		}
		if held != nil {
			_ = held.Close() // not actionable: the fresh grant is in force
		}
		held = next
		maxAge := next.Grant().MaxAge
		if maxAge <= 0 {
			// A grant that never lapses needs no renewal.
			<-ctx.Done()
			return
		}
		lapses = time.Now().Add(maxAge)
		wait = time.Duration(float64(maxAge) * renewAt)
	}
}

// notSupported reports whether err is a relay's answer that it verifies no
// credentials on the session.
func notSupported(err error) bool {
	if authErr, ok := errors.AsType[*moqt.AuthError](err); ok {
		return authErr.Code == moqt.NotSupportedErrorCode
	}
	return errors.Is(err, moqt.ErrAuthNotSupported)
}

// renew signs a credential and presents it, waiting at most presentTimeout
// for the relay's answer.
func renew(ctx context.Context, present func(context.Context, []byte) (credential, error), sign func() (string, time.Time, error)) (credential, error) {
	tok, _, err := sign()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, presentTimeout)
	defer cancel()
	return present(ctx, []byte(tok))
}
