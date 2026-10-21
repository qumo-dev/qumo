package auth

import (
	"io"
)

// low-level utility: ask reads its answer from the stdlib io.Reader seam,
// and a broken one must decline, not panic or hang.
type fakeErrReader struct {
	err error
}

var _ io.Reader = fakeErrReader{}

func (r fakeErrReader) Read([]byte) (int, error) { return 0, r.err }
