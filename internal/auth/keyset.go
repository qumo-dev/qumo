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
	// noPublish holds the kids that start no new sessions that may publish
	// ("publish": false), such as a key whose owner is at a limit on
	// broadcasts. Sessions that only subscribe still start, and live
	// sessions continue.
	noPublish map[string]bool
}

// parseKeySet decodes a JWK Set of Ed25519 public keys as the relay reads it:
// the set token.ParseKeySet takes (each key with its prefix), where a key may
// also carry "publish": false. An empty set is valid and admits nothing, so a
// key-set server with no keys yet is not mistaken for a failing one.
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
		var flags struct {
			Publish *bool `json:"publish"`
		}
		if err := json.Unmarshal(entry, &flags); err != nil {
			return nil, fmt.Errorf("key %d: %w", i, err)
		}
		if flags.Publish == nil || *flags.Publish {
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
