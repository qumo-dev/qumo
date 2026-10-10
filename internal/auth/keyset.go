package auth

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/qumo-dev/qumo/token"
)

// keySet is what a relay verifying credentials itself trusts: the keys by
// kid, each with what it pauses (token.Key.Pause).
type keySet struct {
	keys map[string]token.Key
}

// parseKeySet decodes a JWK Set of Ed25519 public keys as the relay reads it
// (token.ParseKeySet: each key with its prefix and pause). An empty "keys"
// list is valid and admits nothing, so a key-set server with no keys yet is
// not mistaken for a failing one; a set without a "keys" list is refused, so
// the relay keeps its last good set rather than dropping every key.
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
	if len(*set.Keys) == 0 {
		return &keySet{keys: map[string]token.Key{}}, nil
	}
	keys, err := token.ParseKeySet(raw)
	if err != nil {
		return nil, fmt.Errorf("parse key set: %w", err)
	}
	return &keySet{keys: keys}, nil
}
