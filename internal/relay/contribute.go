package relay

import (
	"log/slog"
	"sync"

	"github.com/qumo-dev/gomoqt/moqt"
)

// A publisher asks, on a Contribute Stream, to add one track to a broadcast it
// does not announce. The relay holds the request and subscribes on that stream
// only when a subscriber asks for the track.
//
// A request is served by the relayHandler of the route for exactly its
// broadcast path, so a path takes contributions only while that route lasts.

// maxContributionsPerSession bounds the requests one session may hold open.
const maxContributionsPerSession = 128

type contributionKey struct {
	path moqt.BroadcastPath
	name moqt.TrackName
}

// contribution is one request the relay holds: the track it offers, how to
// answer it, and whose it is. owner is the session's admission, or nil for a
// session without one (a relay peer this relay dialed).
type contribution struct {
	key   contributionKey
	w     *moqt.ContributeResponseWriter
	owner *admission
}

// contributionTable is the requests this relay holds, by broadcast path and
// track name, and how many each session has.
type contributionTable struct {
	mu       sync.Mutex
	held     map[contributionKey]*contribution
	perOwner map[*admission]int
}

func newContributionTable() *contributionTable {
	return &contributionTable{
		held:     make(map[contributionKey]*contribution),
		perOwner: make(map[*admission]int),
	}
}

// add records c and returns the request it replaced, if any. It reports false
// when c's session already holds maxContributionsPerSession.
func (t *contributionTable) add(c *contribution) (replaced *contribution, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c.owner != nil {
		if t.perOwner[c.owner] >= maxContributionsPerSession {
			return nil, false
		}
		t.perOwner[c.owner]++
	}
	replaced = t.held[c.key]
	t.held[c.key] = c
	return replaced, true
}

// remove forgets c. A later request for the same track stays.
func (t *contributionTable) remove(c *contribution) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.held[c.key] == c {
		delete(t.held, c.key)
	}
	if c.owner == nil {
		return
	}
	if n := t.perOwner[c.owner] - 1; n > 0 {
		t.perOwner[c.owner] = n
	} else {
		delete(t.perOwner, c.owner)
	}
}

// get returns how to answer the request for a track, or nil when the relay
// holds none. A nil table holds none.
func (t *contributionTable) get(path moqt.BroadcastPath, name moqt.TrackName) *moqt.ContributeResponseWriter {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if c := t.held[contributionKey{path: path, name: name}]; c != nil {
		return c.w
	}
	return nil
}

// contributionPath is the path a grant must cover for a session to contribute
// name under path: the track as a segment beneath the broadcast. A credential
// that may publish "room/1/chat/alice/**" may therefore contribute the track
// "alice" to /room/1/chat, and may not announce /room/1/chat itself.
func contributionPath(path moqt.BroadcastPath, name moqt.TrackName) moqt.BroadcastPath {
	return moqt.BroadcastPath(string(path) + "/" + string(name))
}

var _ moqt.ContributeHandler = (*relayHandler)(nil)

// ServeContribute admits a request to contribute a track to this route's
// broadcast and holds it until it ends. It does not subscribe: subscribe does,
// when a subscriber asks for the track.
func (h *relayHandler) ServeContribute(w *moqt.ContributeResponseWriter, r *moqt.ContributeRequest) {
	logger := slog.With(
		"node", h.nodeID,
		"broadcast_path", r.BroadcastPath,
		"track_name", r.TrackName,
	)
	if h.contributed == nil {
		w.CloseWithError(moqt.SubscribeErrorCodeNotFound)
		return
	}

	g, err := sessionGrant(r.Context())
	if err != nil {
		return // the request ended before its session was admitted
	}
	// A nil grant is unchecked: a relay peer, or any session with auth off.
	if g != nil && !g.Publish.Contains(contributionPath(r.BroadcastPath, r.TrackName)) {
		metricContributions.WithLabelValues("not_covered").Inc()
		logger.Info("relay: contribution refused: not covered by the session's grant")
		w.CloseWithError(moqt.SubscribeErrorCodeUnauthorized)
		return
	}

	c := &contribution{
		key:   contributionKey{path: r.BroadcastPath, name: r.TrackName},
		w:     w,
		owner: admissionFrom(r.Context()),
	}
	replaced, ok := h.contributed.add(c)
	if !ok {
		metricContributions.WithLabelValues("too_many").Inc()
		logger.Warn("relay: contribution refused: too many on this session")
		w.CloseWithError(moqt.SubscribeErrorCodeInternal)
		return
	}
	defer h.contributed.remove(c)
	if replaced != nil {
		metricContributions.WithLabelValues("replaced").Inc()
		replaced.w.CloseWithError(moqt.SubscribeErrorCodeInternal)
	}
	metricContributions.WithLabelValues("admitted").Inc()
	logger.Debug("relay: contribution admitted")

	// Hold the request while the route and the contributor both last.
	select {
	case <-r.Context().Done():
	case <-h.ctx.Done():
	}
}
