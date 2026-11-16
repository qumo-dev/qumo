package relaysign

import "time"

// SetNow sets the clock s signs with, for the external tests.
func (s *Signer) SetNow(now func() time.Time) { s.now = now }
