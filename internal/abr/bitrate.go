package abr

// bitrateGroups is how many groups a bitrateMeter measures before it states
// a bitrate.
const bitrateGroups = 3

// A bitrateMeter measures a video track's bitrate from its first few groups,
// to state the source's bitrate in the catalog alongside the renditions'
// target bitrates. A player choosing between them needs every alternative's.
//
// It states the highest group bitrate it saw, as MSF's bitrate is a maximum
// (draft-ietf-moq-msf-01 §5.2.22), once.
type bitrateMeter struct {
	groups int    // groups measured
	start  uint64 // timestamp of the group being measured, in µs
	bytes  int64  // bytes of the group being measured
	max    int64  // highest group bitrate, in bit/s
	done   bool
}

func newBitrateMeter() *bitrateMeter {
	return &bitrateMeter{}
}

// add counts a frame of n bytes stamped ts microseconds; first reports that
// it opens a group. When the meter has measured enough groups it returns the
// bitrate to state, and true, once.
func (m *bitrateMeter) add(ts uint64, n int, first bool) (int64, bool) {
	if m.done {
		return 0, false
	}
	if first {
		if m.bytes > 0 && ts > m.start {
			bps := m.bytes * 8 * 1_000_000 / int64(ts-m.start)
			m.max = max(m.max, bps)
			m.groups++
		}
		m.start, m.bytes = ts, 0
		if m.groups == bitrateGroups {
			m.done = true
			return m.max, true
		}
	}
	m.bytes += int64(n)
	return 0, false
}
