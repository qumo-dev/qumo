// Package trust holds a managed relay's view of which signing keys and
// projects it may admit: the trust snapshot the control plane publishes at
// GET /v1/relays/trust (qumo-deploy ADR 0035, Decision 5).
//
// Store answers two questions. Key: may a new session be admitted under this
// kid? Live: may an already admitted session continue? Poller keeps the Store
// current by polling the control plane.
package trust

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/qumo-dev/qumo/internal/credential"
)

// maxStaleness is how long new sessions are still admitted after polls start
// failing (fail-static). Live sessions are not affected by staleness.
const maxStaleness = 6 * time.Hour

// Key states in the snapshot.
const (
	stateActive  = "active"
	stateRetired = "retired"
	stateRevoked = "revoked"
)

// Reasons a live session must end.
const (
	ReasonKeyRevoked       = "key_revoked"
	ReasonProjectSuspended = "project_suspended"
)

var (
	errNotLoaded        = errors.New("trust: no snapshot loaded yet")
	errStale            = errors.New("trust: snapshot too stale to admit new sessions")
	errUnknownKey       = errors.New("trust: signing key not in the snapshot")
	errKeyNotActive     = errors.New("trust: signing key is not active")
	errProjectSuspended = errors.New("trust: project is suspended")
)

// Snapshot is the control plane's trust snapshot, as served.
type Snapshot struct {
	Keys     []SnapshotKey `json:"keys"`
	Policies []Policy      `json:"policies"`
}

// SnapshotKey is one signing key in the snapshot.
type SnapshotKey struct {
	ID        string `json:"kid"`
	X         string `json:"x"`
	ProjectID string `json:"project_id"`
	Prefix    string `json:"prefix"`
	State     string `json:"state"`
}

// Policy is one project's admission policy.
type Policy struct {
	ProjectID string `json:"project_id"`
	Suspended bool   `json:"suspended"`
	Quotas    Quotas `json:"quotas"`
}

// Quotas are a project's service quotas; nil means unlimited.
type Quotas struct {
	Broadcasts         *int `json:"broadcasts"`
	SubscriberSessions *int `json:"subscriber_sessions"`
}

// entry is a parsed snapshot key.
type entry struct {
	key   credential.Key
	state string
}

// state is one immutable, parsed snapshot.
type state struct {
	keys     map[string]entry
	policies map[string]Policy
}

// Store is the current trust snapshot. The zero value holds no snapshot and
// admits nothing. It is safe for concurrent use.
type Store struct {
	now func() time.Time

	current     atomic.Pointer[state]
	lastSuccess atomic.Int64 // unix nanoseconds of the last successful poll

	mu        sync.Mutex
	listeners []func()
}

var _ credential.Keys = (*Store)(nil)

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{now: time.Now}
}

// Key returns the key for kid if a new session may be admitted under it: a
// snapshot is loaded and fresh, the key is active, and its project is not
// suspended.
func (s *Store) Key(_ context.Context, kid string) (credential.Key, error) {
	st := s.current.Load()
	if st == nil {
		return credential.Key{}, errNotLoaded
	}
	if s.now().Sub(time.Unix(0, s.lastSuccess.Load())) > maxStaleness {
		return credential.Key{}, errStale
	}
	e, ok := st.keys[kid]
	if !ok {
		return credential.Key{}, errUnknownKey
	}
	if e.state != stateActive {
		return credential.Key{}, fmt.Errorf("%w: %s", errKeyNotActive, e.state)
	}
	if st.policies[e.key.ProjectID].Suspended {
		return credential.Key{}, errProjectSuspended
	}
	return e.key, nil
}

// Live reports whether a session admitted under kid for projectID may
// continue, and if not, why. A retired key keeps its sessions; a revoked or
// removed key, or a suspended project, ends them. Staleness does not.
func (s *Store) Live(kid, projectID string) (reason string, ok bool) {
	st := s.current.Load()
	if st == nil {
		return "", true
	}
	if e, found := st.keys[kid]; !found || e.state == stateRevoked {
		return ReasonKeyRevoked, false
	}
	if st.policies[projectID].Suspended {
		return ReasonProjectSuspended, false
	}
	return "", true
}

// Quotas returns projectID's service quotas from the current snapshot; a nil
// quota, or no snapshot, means unlimited.
func (s *Store) Quotas(projectID string) Quotas {
	st := s.current.Load()
	if st == nil {
		return Quotas{}
	}
	return st.policies[projectID].Quotas
}

// OnChange registers fn to run after every snapshot swap.
func (s *Store) OnChange(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners = append(s.listeners, fn)
}

// replace swaps in snap atomically and notifies listeners. A key that cannot
// be used (bad x, kid not its thumbprint, unknown state, no prefix) is skipped
// and logged: one bad entry must not take down admission for every project.
func (s *Store) replace(snap Snapshot) {
	st := &state{
		keys:     make(map[string]entry, len(snap.Keys)),
		policies: make(map[string]Policy, len(snap.Policies)),
	}
	for _, k := range snap.Keys {
		switch k.State {
		case stateActive, stateRetired, stateRevoked:
		default:
			slog.Warn("trust: skipping key with unknown state", "kid", k.ID, "state", k.State)
			continue
		}
		// An empty prefix would leave the key unconstrained, able to sign for
		// every tenant. A managed key is always confined to its project.
		if strings.Trim(k.Prefix, "/") == "" {
			slog.Warn("trust: skipping key without a prefix", "kid", k.ID, "project_id", k.ProjectID)
			continue
		}
		key, err := credential.NewKey(k.ID, k.X)
		if err != nil {
			slog.Warn("trust: skipping unusable key", "kid", k.ID, "error", err)
			continue
		}
		key.ProjectID = k.ProjectID
		key.Prefix = k.Prefix
		st.keys[key.ID] = entry{key: key, state: k.State}
	}
	for _, p := range snap.Policies {
		st.policies[p.ProjectID] = p
	}
	s.current.Store(st)
	s.markFresh()
	metricKeys.Set(float64(len(st.keys)))

	s.mu.Lock()
	listeners := append([]func(){}, s.listeners...)
	s.mu.Unlock()
	for _, fn := range listeners {
		fn()
	}
}

// markFresh records a successful poll, including an unchanged (304) one.
func (s *Store) markFresh() {
	now := s.now()
	s.lastSuccess.Store(now.UnixNano())
	metricLastSuccess.Set(float64(now.Unix()))
}
