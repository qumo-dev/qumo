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
	keep(sess.Context(), func(ctx context.Context, tok []byte) (credential, error) {
		c, err := sess.Auth(ctx, tok)
		if err != nil {
			return nil, err
		}
		return c, nil
	}, func() {
		// not actionable: the session is ending either way.
		_ = sess.CloseWithError(moqt.UnauthorizedSessionErrorCode, "relay credential lapsed")
	}, func() (string, time.Time, error) { return Sign(key, g) }, lapses)
}

// keep is Keep, presenting with present and closing the session with end.
func keep(ctx context.Context, present func(context.Context, []byte) (credential, error), end func(),
	sign func() (string, time.Time, error), lapses time.Time) {
	var held credential
	defer func() {
		if held != nil {
			_ = held.Close() // not actionable: the session is ending
		}
	}()
	wait := time.Duration(float64(time.Until(lapses)) * renewAt)
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
				slog.Info("relay credential not renewed: the relay verifies none, and the session's grant does not lapse")
				return
			}
			// A retry signs, then may wait presentTimeout for an answer: the
			// session must not outlive its grant meanwhile.
			if time.Until(lapses) <= retryInterval+presentTimeout {
				slog.Error("relay credential lapsing: closing the session to dial again", "error", err)
				end()
				return
			}
			slog.Warn("relay credential not renewed, retrying", "error", err, "lapses_in", time.Until(lapses).Round(time.Second))
			wait = retryInterval
			continue
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
