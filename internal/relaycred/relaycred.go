// Package relaycred keeps a relay session's grant in force for a client that
// signs its own credentials, such as the funnel and the HLS egress. A relay
// grants a credential until its token expires, so a session that lives
// longer presents a fresh one on its Auth Stream before then.
package relaycred

import (
	"context"
	"log/slog"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
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

// credential is a token presented on an Auth Stream, as keeping a grant in
// force needs one: *moqt.Credential.
type credential interface {
	Grant() moqt.Grant
	Close() error
}

// Keep presents a credential sign returns on sess's Auth Stream each time the
// grant in force is two thirds through its lifetime, until sess ends. The
// first grant is the one sess connected with, which lasts lifetime. A
// credential that can't be signed or is not granted is tried again every
// retryInterval; when the grant in force lapses first, sess is closed, so that
// its owner dials again with a fresh credential rather than hold a session
// that may do nothing.
func Keep(sess *moqt.Session, sign func() (string, error), lifetime time.Duration) {
	keep(sess.Context(), func(ctx context.Context, tok []byte) (credential, error) {
		c, err := sess.Auth(ctx, tok)
		if err != nil {
			return nil, err
		}
		return c, nil
	}, func() {
		// not actionable: the session is ending either way.
		_ = sess.CloseWithError(moqt.UnauthorizedSessionErrorCode, "relay credential lapsed")
	}, sign, lifetime)
}

// keep is Keep, presenting with present and closing the session with end.
func keep(ctx context.Context, present func(context.Context, []byte) (credential, error), end func(),
	sign func() (string, error), lifetime time.Duration) {
	var held credential
	defer func() {
		if held != nil {
			_ = held.Close() // not actionable: the session is ending
		}
	}()
	lapses := time.Now().Add(lifetime)
	wait := time.Duration(float64(lifetime) * renewAt)
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
			if time.Until(lapses) <= retryInterval {
				slog.Error("relay credential lapsed: closing the session to dial again", "error", err)
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

// renew signs a credential and presents it, waiting at most presentTimeout
// for the relay's answer.
func renew(ctx context.Context, present func(context.Context, []byte) (credential, error), sign func() (string, error)) (credential, error) {
	tok, err := sign()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, presentTimeout)
	defer cancel()
	return present(ctx, []byte(tok))
}
