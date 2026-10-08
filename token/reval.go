package token

import (
	"fmt"
	"time"
)

// MinReval is the shortest reval a token may carry: a relay re-checks its
// live sessions every 30 s, so it can honor no shorter interval. A token
// asking for one is refused rather than revalidated less often than it asks.
const MinReval = 30 * time.Second

// An Option sets an optional claim of a token Sign makes.
type Option func(*claims) error

// WithReval makes the token one a relay revalidates for as long as the
// session it admitted lives, at least every interval, in whole seconds,
// at least MinReval and at most MaxLifetime. The session ends when the token
// no longer verifies, at the latest at its expiry. Without it, the token's
// expiry decides only whether a session may start.
func WithReval(interval time.Duration) Option {
	return func(c *claims) error {
		if interval < MinReval || interval > MaxLifetime {
			return fmt.Errorf("token: reval %s must be at least %s and at most %s", interval, MinReval, MaxLifetime)
		}
		secs := float64(interval / time.Second)
		c.Reval = &secs
		return nil
	}
}

// VerifyLive checks token as the credential of a live session it admitted,
// at now: as Verify does, except that a token without reval is not refused
// for having expired. Its expiry decided whether the session could start; the
// key that signed it must still be trusted, and its grant still within the
// key's prefix. A token with reval is checked exactly as Verify checks it.
func VerifyLive(token string, keys map[string]Key, now time.Time) (Claims, error) {
	return verify(token, keys, now, true)
}

// revalOf returns the reval claim as an interval, zero when the token carries
// none. A value the verifier can't honor (below MinReval) or that is never
// due within a token's lifetime (above MaxLifetime) makes the token invalid.
func (c claims) revalOf() (time.Duration, error) {
	if c.Reval == nil {
		return 0, nil
	}
	secs := *c.Reval
	if secs < MinReval.Seconds() || secs > MaxLifetime.Seconds() {
		return 0, invalid("reval %v s must be at least %v s and at most %v s", secs, MinReval.Seconds(), MaxLifetime.Seconds())
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// judgesExpiry reports whether a check of c refuses it once expired: always
// at admission, and for a live session only when the token carries reval.
func (c claims) judgesExpiry(live bool) bool {
	return !live || c.Reval != nil
}
