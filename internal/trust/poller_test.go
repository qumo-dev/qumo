package trust

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeControlPlane serves a trust snapshot with an ETag and records requests.
type fakeControlPlane struct {
	mu       sync.Mutex
	snapshot Snapshot
	etag     string
	status   int // non-zero overrides the response status
	requests []*http.Request
}

func (f *fakeControlPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	if r.URL.Path != trustPath {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if f.etag != "" && r.Header.Get("If-None-Match") == f.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", f.etag)
	_ = json.NewEncoder(w).Encode(f.snapshot)
}

func (f *fakeControlPlane) set(snap Snapshot, etag string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshot, f.etag, f.status = snap, etag, status
}

func (f *fakeControlPlane) lastRequest() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func newTestPoller(t *testing.T, cp *fakeControlPlane) (*Poller, *Store, *time.Time) {
	t.Helper()
	srv := httptest.NewServer(cp)
	t.Cleanup(srv.Close)
	now := time.Unix(1_800_000_000, 0)
	store := &Store{now: func() time.Time { return now }}
	return NewPoller(srv.URL, "relay-token", srv.Client(), store), store, &now
}

func TestPoller_LoadsSnapshotWithToken(t *testing.T) {
	k := newTestKey(t)
	cp := &fakeControlPlane{}
	cp.set(Snapshot{Keys: []SnapshotKey{k.entry("p1", stateActive)}}, `"v1"`, 0)
	p, store, _ := newTestPoller(t, cp)

	err := p.Poll(t.Context())

	require.NoError(t, err)
	assert.Equal(t, "Bearer relay-token", cp.lastRequest().Header.Get("Authorization"))
	_, err = store.Key(t.Context(), k.kid)
	assert.NoError(t, err)
}

func TestPoller_NotModifiedKeepsSnapshotAndRefreshesFreshness(t *testing.T) {
	k := newTestKey(t)
	cp := &fakeControlPlane{}
	cp.set(Snapshot{Keys: []SnapshotKey{k.entry("p1", stateActive)}}, `"v1"`, 0)
	p, store, now := newTestPoller(t, cp)
	require.NoError(t, p.Poll(t.Context()))
	changes := 0
	store.OnChange(func() { changes++ })

	*now = now.Add(maxStaleness)
	require.NoError(t, p.Poll(t.Context()))
	*now = now.Add(time.Hour)

	assert.Equal(t, `"v1"`, cp.lastRequest().Header.Get("If-None-Match"))
	assert.Zero(t, changes, "a 304 does not swap the snapshot")
	_, err := store.Key(t.Context(), k.kid)
	assert.NoError(t, err, "the 304 counted as a successful poll")
}

func TestPoller_NewSnapshotReplacesTheOld(t *testing.T) {
	k := newTestKey(t)
	cp := &fakeControlPlane{}
	cp.set(Snapshot{Keys: []SnapshotKey{k.entry("p1", stateActive)}}, `"v1"`, 0)
	p, store, _ := newTestPoller(t, cp)
	require.NoError(t, p.Poll(t.Context()))

	cp.set(Snapshot{}, `"v2"`, 0)
	require.NoError(t, p.Poll(t.Context()))

	reason, ok := store.Live(k.kid, "p1")
	assert.False(t, ok)
	assert.Equal(t, ReasonKeyRevoked, reason)
}

func TestPoller_FailureKeepsLastSnapshot(t *testing.T) {
	k := newTestKey(t)
	cp := &fakeControlPlane{}
	cp.set(Snapshot{Keys: []SnapshotKey{k.entry("p1", stateActive)}}, `"v1"`, 0)
	p, store, _ := newTestPoller(t, cp)
	require.NoError(t, p.Poll(t.Context()))

	cp.set(Snapshot{}, `"v2"`, http.StatusServiceUnavailable)
	err := p.Poll(t.Context())

	assert.Error(t, err)
	_, err = store.Key(t.Context(), k.kid)
	assert.NoError(t, err)
}

func TestPoller_NotModifiedBeforeAnyLoadRefetches(t *testing.T) {
	cp := &fakeControlPlane{}
	cp.set(Snapshot{}, `"v1"`, 0)
	p, store, _ := newTestPoller(t, cp)
	p.etag = `"v1"`

	err := p.Poll(t.Context())
	require.Error(t, err)
	require.NoError(t, p.Poll(t.Context()))

	_, err = store.Key(t.Context(), "any")
	assert.ErrorIs(t, err, errUnknownKey, "the second poll loaded the snapshot")
}
