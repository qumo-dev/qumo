package auth

import (
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"github.com/qumo-dev/qumo/token"
)

// keySet is what a relay verifying credentials itself trusts: the keys by
// kid, and which of them start no new publishing sessions.
type keySet struct {
	keys map[string]token.Key
	// pausedPublish holds the kids whose publishing is paused ("pause":
	// ["publish"]): they start no new sessions that may publish, such as a
	// key whose owner is at a limit on broadcasts. Sessions that only
	// subscribe still start, and live sessions continue.
	pausedPublish map[string]bool
	// deprecatedPublish lists, sorted, the kids paused with the deprecated
	// "publish": false.
	deprecatedPublish []string
}

// pausePublish is the "pause" entry that stops a key's new publishing
// sessions. It is the only thing a key can pause.
const pausePublish = "publish"

// keyMembers are the members a key in a set may carry beyond the JWK ones the
// token package reads, and the public key that finds the key's kid.
type keyMembers struct {
	// X is the key's base64url public key.
	X string `json:"x"`
	// Pause lists what the key starts no new sessions for.
	Pause []string `json:"pause"`
	// Publish false is the deprecated spelling of "pause": ["publish"].
	Publish *bool `json:"publish"`
}

// deprecatedPause reports whether the key is paused with the deprecated
// "publish": false.
func (m keyMembers) deprecatedPause() bool {
	return m.Publish != nil && !*m.Publish
}

// pausesPublish reports whether a key's members pause its publishing. It
// refuses an entry in "pause" the relay doesn't know, so a misspelled entry
// can't leave a key unpaused, and "publish": true beside a pause of
// publishing, which contradicts it.
func (m keyMembers) pausesPublish() (bool, error) {
	paused := m.deprecatedPause()
	for _, p := range m.Pause {
		if p != pausePublish {
			return false, fmt.Errorf("pause %q: only %q can be paused", p, pausePublish)
		}
		paused = true
	}
	if paused && m.Publish != nil && *m.Publish {
		return false, fmt.Errorf(`"publish": true contradicts "pause": [%q]`, pausePublish)
	}
	return paused, nil
}

// parseKeySet decodes a JWK Set of Ed25519 public keys as the relay reads it:
// the set token.ParseKeySet takes (each key with its prefix), where a key may
// also carry "pause": ["publish"]. Member names are case-sensitive and an
// object naming a member twice is refused, as in token.ParseKeySet. An empty
// "keys" list is valid and admits nothing, so a key-set server with no keys
// yet is not mistaken for a failing one; a set without a "keys" list is
// refused, so the relay keeps its last good set rather than dropping every
// key.
func parseKeySet(raw []byte) (*keySet, error) {
	var set struct {
		Keys *[]jsontext.Value `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("decode key set: %w", err)
	}
	if set.Keys == nil {
		return nil, errors.New(`decode key set: no "keys" list`)
	}
	ks := &keySet{keys: map[string]token.Key{}, pausedPublish: map[string]bool{}}
	if len(*set.Keys) == 0 {
		return ks, nil
	}
	// token.ParseKeySet validates every key and refuses one listed twice.
	keys, err := token.ParseKeySet(raw)
	if err != nil {
		return nil, fmt.Errorf("parse key set: %w", err)
	}
	ks.keys = keys
	// An entry's kid is the thumbprint of its public key, which ParseKeySet
	// computes whether or not the entry names it; the key finds the kid.
	kids := make(map[string]string, len(keys))
	for kid, k := range keys {
		kids[string(k.Public)] = kid
	}
	for i, entry := range *set.Keys {
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
		x, err := base64.RawURLEncoding.DecodeString(members.X)
		if err != nil {
			return nil, fmt.Errorf("key %d: decode x: %w", i, err)
		}
		kid, ok := kids[string(x)]
		if !ok {
			return nil, fmt.Errorf("key %d: not in the parsed key set", i)
		}
		ks.pausedPublish[kid] = true
		if members.deprecatedPause() {
			ks.deprecatedPublish = append(ks.deprecatedPublish, kid)
		}
	}
	slices.Sort(ks.deprecatedPublish)
	return ks, nil
}
