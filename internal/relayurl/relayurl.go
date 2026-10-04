// Package relayurl keeps a relay URL's credential out of logs. A client
// connects with its credential in the URL's query (?jwt=…), so a relay URL is
// never logged whole: only its scheme, host and path.
package relayurl

import (
	"net/url"
	"strings"
)

// Redact returns raw without its query, fragment and user info, fit to log.
// A URL that doesn't parse is reduced to the part before any "?".
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		before, _, _ := strings.Cut(raw, "?")
		return before
	}
	u.RawQuery, u.ForceQuery = "", false
	u.Fragment, u.RawFragment = "", ""
	u.User = nil
	return u.String()
}

// ScrubError returns err with raw's query removed from its message, for an
// error from dialing raw that may quote it. The original error stays in the
// chain, so errors.Is and errors.As still see it. A nil err, or a raw with no
// query, is returned as is.
func ScrubError(err error, raw string) error {
	if err == nil {
		return nil
	}
	u, perr := url.Parse(raw)
	if perr != nil || u.RawQuery == "" {
		return err
	}
	return &scrubbedError{err: err, query: "?" + u.RawQuery}
}

// scrubbedError is an error whose message leaves out a URL query.
type scrubbedError struct {
	err   error
	query string
}

func (e *scrubbedError) Error() string {
	return strings.ReplaceAll(e.err.Error(), e.query, "")
}

func (e *scrubbedError) Unwrap() error {
	return e.err
}
