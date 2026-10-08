package auth

import (
	"encoding/json"
	"fmt"

	"github.com/qumo-dev/qumo/token"
)

// keySet is what a relay verifying credentials itself trusts: the keys by
// kid, and which of them start no new publishing sessions.
type keySet struct {
	keys map[string]token.Key
	// noPublish holds the kids whose publishing is paused ("pause":
	// ["publish"]): they start no new sessions that may publish, such as a
	// key whose owner is at a limit on broadcasts. Sessions that only
	// subscribe still start, and live sessions continue.
	noPublish map[string]bool
}

// pausePublish is the "pause" entry that stops a key's new publishing
// sessions. It is the only thing a key can pause.
const pausePublish = "publish"

// keyMembers are the members a key in a set may carry beyond the JWK ones the
// token package reads.
type keyMembers struct {
	// Pause lists what the key starts no new sessions for.
	Pause []string `json:"pause"`
	// Publish false is the deprecated spelling of "pause": ["publish"].
	Publish *bool `json:"publish"`
}

// pausesPublish reports whether a key's members pause its publishing, and
// refuses a pause the relay doesn't know, so a misspelled entry can't leave
// a key unpaused.
func (m keyMembers) pausesPublish() (bool, error) {
	paused := m.Publish != nil && !*m.Publish
	for _, p := range m.Pause {
		if p != pausePublish {
			return false, fmt.Errorf("pause %q: only %q can be paused", p, pausePublish)
		}
		paused = true
	}
	return paused, nil
}

// parseKeySet decodes a JWK Set of Ed25519 public keys as the relay reads it:
// the set token.ParseKeySet takes (each key with its prefix), where a key may
// also carry "pause": ["publish"]. An empty set is valid and admits nothing, so
// a key-set server with no keys yet is not mistaken for a failing one.
func parseKeySet(raw []byte) (*keySet, error) {
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("decode key set: %w", err)
	}
	ks := &keySet{keys: map[string]token.Key{}, noPublish: map[string]bool{}}
	if len(set.Keys) == 0 {
		return ks, nil
	}
	// token.ParseKeySet validates every key and refuses one listed twice.
	keys, err := token.ParseKeySet(raw)
	if err != nil {
		return nil, err
	}
	ks.keys = keys
	for i, entry := range set.Keys {
		var members keyMembers
		if err := json.Unmarshal(entry, &members); err != nil {
			return nil, fmt.Errorf("key %d: %w", i, err)
		}
		paused, err := members.pausesPublish()
		if err != nil {
			return nil, fmt.Errorf("key %d: %w", i, err)
		}
		if !paused {
			continue
		}
		// The entry's kid is its thumbprint, which ParseKeySet computes;
		// parsing it alone gives that kid whether or not the entry names it.
		one, err := token.ParseKeySet([]byte(`{"keys":[` + string(entry) + `]}`))
		if err != nil {
			return nil, fmt.Errorf("key %d: %w", i, err)
		}
		for kid := range one {
			ks.noPublish[kid] = true
		}
	}
	return ks, nil
}
