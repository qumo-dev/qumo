package trust

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
	"time"

	"github.com/qumo-dev/qumo/internal/credential"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testKey is a fresh Ed25519 key in snapshot form.
type testKey struct {
	kid string
	x   string
}

func newTestKey(t *testing.T) testKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	x := base64.RawURLEncoding.EncodeToString(pub)
	key, err := credential.NewKey("", x)
	require.NoError(t, err)
	return testKey{kid: key.ID, x: x}
}

func (k testKey) entry(project, state string) SnapshotKey {
	return SnapshotKey{ID: k.kid, X: k.x, ProjectID: project, Prefix: project + "/live", State: state}
}

// newLoadedStore returns a Store at a fixed clock holding snap.
func newLoadedStore(snap Snapshot) (*Store, *time.Time) {
	now := time.Unix(1_800_000_000, 0)
	s := &Store{now: func() time.Time { return now }}
	s.replace(snap)
	return s, &now
}

func TestStore_Key(t *testing.T) {
	active, retired, revoked, suspended := newTestKey(t), newTestKey(t), newTestKey(t), newTestKey(t)
	s, _ := newLoadedStore(Snapshot{
		Keys: []SnapshotKey{
			active.entry("p1", stateActive),
			retired.entry("p1", stateRetired),
			revoked.entry("p1", stateRevoked),
			suspended.entry("p2", stateActive),
		},
		Policies: []Policy{{ProjectID: "p2", Suspended: true}},
	})

	tests := map[string]struct {
		kid     string
		wantErr error
	}{
		"active key":        {kid: active.kid},
		"retired key":       {kid: retired.kid, wantErr: errKeyNotActive},
		"revoked key":       {kid: revoked.kid, wantErr: errKeyNotActive},
		"unknown key":       {kid: "nope", wantErr: errUnknownKey},
		"suspended project": {kid: suspended.kid, wantErr: errProjectSuspended},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			key, err := s.Key(t.Context(), tt.kid)

			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.kid, key.ID)
			assert.Equal(t, "p1", key.ProjectID)
			assert.Equal(t, "p1/live", key.Prefix)
		})
	}
}

func TestStore_Key_NothingLoaded(t *testing.T) {
	_, err := NewStore().Key(t.Context(), "kid")

	assert.ErrorIs(t, err, errNotLoaded)
}

func TestStore_Key_FailStatic(t *testing.T) {
	k := newTestKey(t)
	s, now := newLoadedStore(Snapshot{Keys: []SnapshotKey{k.entry("p1", stateActive)}})

	*now = now.Add(maxStaleness)
	_, err := s.Key(t.Context(), k.kid)
	require.NoError(t, err, "exactly at the limit still admits")

	*now = now.Add(time.Second)
	_, err = s.Key(t.Context(), k.kid)
	assert.ErrorIs(t, err, errStale)

	_, ok := s.Live(k.kid, "p1")
	assert.True(t, ok, "staleness never ends live sessions")
}

func TestStore_Live(t *testing.T) {
	active, retired, revoked := newTestKey(t), newTestKey(t), newTestKey(t)
	s, _ := newLoadedStore(Snapshot{
		Keys: []SnapshotKey{
			active.entry("p1", stateActive),
			retired.entry("p1", stateRetired),
			revoked.entry("p1", stateRevoked),
		},
		Policies: []Policy{{ProjectID: "p2", Suspended: true}},
	})

	tests := map[string]struct {
		kid, project string
		wantReason   string
	}{
		"active key":        {kid: active.kid, project: "p1"},
		"retired key":       {kid: retired.kid, project: "p1"},
		"revoked key":       {kid: revoked.kid, project: "p1", wantReason: ReasonKeyRevoked},
		"key gone":          {kid: "gone", project: "p1", wantReason: ReasonKeyRevoked},
		"suspended project": {kid: active.kid, project: "p2", wantReason: ReasonProjectSuspended},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			reason, ok := s.Live(tt.kid, tt.project)

			assert.Equal(t, tt.wantReason == "", ok)
			assert.Equal(t, tt.wantReason, reason)
		})
	}
}

func TestStore_ReplaceSkipsUnusableKeys(t *testing.T) {
	good, other := newTestKey(t), newTestKey(t)
	s, _ := newLoadedStore(Snapshot{Keys: []SnapshotKey{
		good.entry("p1", stateActive),
		{ID: other.kid, X: "not-a-key", ProjectID: "p1", State: stateActive},
		{ID: "not-the-thumbprint", X: other.x, ProjectID: "p1", State: stateActive},
		{ID: other.kid, X: other.x, ProjectID: "p1", Prefix: "p1/live", State: "paused"},
		{ID: other.kid, X: other.x, ProjectID: "p1", Prefix: "", State: stateActive},
		{ID: other.kid, X: other.x, ProjectID: "p1", Prefix: "/", State: stateActive},
	}})

	_, err := s.Key(t.Context(), good.kid)
	require.NoError(t, err)
	_, err = s.Key(t.Context(), other.kid)
	assert.ErrorIs(t, err, errUnknownKey)
}

func TestStore_OnChange(t *testing.T) {
	s := NewStore()
	calls := 0
	s.OnChange(func() { calls++ })

	s.replace(Snapshot{})
	s.replace(Snapshot{})

	assert.Equal(t, 2, calls)
}

func TestStore_Quotas(t *testing.T) {
	two := 2
	s, _ := newLoadedStore(Snapshot{Policies: []Policy{{ProjectID: "p1", Quotas: Quotas{Broadcasts: &two}}}})

	assert.Equal(t, &two, s.Quotas("p1").Broadcasts)
	assert.Nil(t, s.Quotas("p1").SubscriberSessions, "an absent quota is unlimited")
	assert.Equal(t, Quotas{}, s.Quotas("unknown"))
	assert.Equal(t, Quotas{}, NewStore().Quotas("p1"), "no snapshot: unlimited")
}
