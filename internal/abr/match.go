package abr

import (
	"sync"

	"github.com/qumo-dev/gomoqt/moqt"
)

// frameInfo is what a rendition frame inherits from the source frame it was
// encoded from: where the source frame sits in its track, and its timestamps.
type frameInfo struct {
	// Group is the sequence of the source group the frame belongs to.
	Group moqt.GroupSequence
	// First reports whether the frame opens its group.
	First bool
	// Timestamp is the frame's LOC timestamp, in microseconds.
	Timestamp uint64
	// WireTimestamp is the moqt frame timestamp, in the track's timescale.
	WireTimestamp uint64
}

// matchTolerance is how far apart, in microseconds, an encoder output
// timestamp and a source timestamp may be and still name the same frame. FLV
// carries milliseconds, which ffmpeg rounds to the nearest, so a match is
// never more than half a millisecond off; frames are tens of milliseconds
// apart, so a millisecond cannot name the wrong one.
const matchTolerance = 1000

// maxPending bounds the frames a matcher remembers. An encoder that falls
// this far behind has stopped; the oldest are forgotten rather than kept
// without bound.
const maxPending = 1024

// A matcher pairs the frames an encoder outputs with the source frames they
// were encoded from.
//
// The encoder passes frames through one for one (no B-frames, no frame rate
// conversion), so outputs come back in the order frames were fed, and
// matching is by order. The output's timestamp, in milliseconds, confirms
// each match, and lets the matcher step over a source frame the encoder
// dropped instead of shifting every later frame onto the wrong source.
type matcher struct {
	mu      sync.Mutex
	pending []pendingFrame
}

// pendingFrame is a source frame fed to the encoder and not yet matched.
type pendingFrame struct {
	// fed is the timestamp the encoder was given, in microseconds.
	fed  uint64
	info frameInfo
}

// push records a source frame about to be fed to the encoder stamped fed
// microseconds.
func (m *matcher) push(fed uint64, f frameInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pending) == maxPending {
		m.pending = m.pending[1:]
	}
	m.pending = append(m.pending, pendingFrame{fed: fed, info: f})
}

// head returns the timestamp the oldest source frame remembered was fed
// with, for diagnostics, and false when none is.
func (m *matcher) head() (uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pending) == 0 {
		return 0, false
	}
	return m.pending[0].fed, true
}

// match returns the source frame an output frame stamped ms milliseconds was
// encoded from, and forgets it and any older source frame the encoder
// skipped. It reports false when no remembered source frame matches.
func (m *matcher) match(ms uint32) (frameInfo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := int64(ms) * 1000
	for len(m.pending) > 0 {
		head := m.pending[0]
		d := out - int64(head.fed)
		switch {
		case d >= matchTolerance:
			// The output is later than this source frame: the encoder
			// dropped it.
			m.pending = m.pending[1:]
		case d <= -matchTolerance:
			// The output is earlier than every source frame remembered.
			return frameInfo{}, false
		default:
			m.pending = m.pending[1:]
			return head.info, true
		}
	}
	return frameInfo{}, false
}
