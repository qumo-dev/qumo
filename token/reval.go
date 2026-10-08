package token

import "time"

// revalInterval is how often a qumo relay re-checks a live session, and so the
// interval Sign writes as reval: the relay revalidates every session at least
// that often. A reval asking for a shorter interval is one the relay can't
// honor, and Verify refuses it.
const revalInterval = 30 * time.Second

// VerifyLive checks token as the credential of a live session it admitted,
// at now: as Verify does, except that a token without reval is not refused
// for having expired. Its expiry decided whether the session could start; the
// key that signed it must still be trusted, and its grant still within the
// key's prefix. A token with reval is checked exactly as Verify checks it.
func VerifyLive(token string, keys map[string]Key, now time.Time) (Claims, error) {
	return verify(token, keys, now, true)
}

// checkReval refuses a reval interval shorter than a relay re-checks at, which
// it could not honor. A longer one is honored by re-checking more often than
// asked, so there is no upper bound.
func (c claims) checkReval() error {
	if c.Reval != nil && *c.Reval < revalInterval.Seconds() {
		return invalid("reval %v s is shorter than the %v s a relay revalidates at", *c.Reval, revalInterval.Seconds())
	}
	return nil
}

// judgesExpiry reports whether a check of c refuses it once expired: always
// at admission, and for a live session only when the token carries reval.
func (c claims) judgesExpiry(live bool) bool {
	return !live || c.Reval != nil
}
