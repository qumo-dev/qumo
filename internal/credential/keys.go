package credential

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var errNoKeys = errors.New("keys: the key set lists no keys")

// Key is a signing key a relay trusts.
type Key struct {
	// ID is the kid: the key's RFC 7638 thumbprint.
	ID     string
	Public ed25519.PublicKey
	// ProjectID is the qumo project the key belongs to; empty for a key
	// configured statically on the relay.
	ProjectID string
	// Prefix is the broadcast-path prefix the key may sign for; empty leaves
	// it unconstrained.
	Prefix string
}

// NewKey builds a Key from its base64url Ed25519 public key x. A non-empty kid
// must equal the key's RFC 7638 thumbprint; an empty one is derived.
func NewKey(kid, x string) (Key, error) {
	pub, err := base64.RawURLEncoding.DecodeString(x)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return Key{}, errors.New("x is not a base64url Ed25519 public key")
	}
	want := thumbprint(x)
	if kid != "" && kid != want {
		return Key{}, fmt.Errorf("kid %q is not the key's RFC 7638 thumbprint %q", kid, want)
	}
	return Key{ID: want, Public: ed25519.PublicKey(pub)}, nil
}

// StaticKeys is a fixed key set by kid: the public keys a relay operator lists
// in the relay's own configuration. It never changes while the relay runs.
type StaticKeys map[string]ed25519.PublicKey

var _ Keys = StaticKeys(nil)

// Key returns the key for kid. An unknown kid is refused, with no lookup
// elsewhere.
func (k StaticKeys) Key(_ context.Context, kid string) (Key, error) {
	pub, ok := k[kid]
	if !ok {
		return Key{}, errUnknownKey
	}
	return Key{ID: kid, Public: pub}, nil
}

// jwk is one Ed25519 public key in JWK form (RFC 8037).
type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid"`
}

// LoadKeys reads a JWK Set of Ed25519 public keys from path. A key without a
// kid gets its RFC 7638 thumbprint, the kid qumo assigns at registration; a
// key whose kid differs from its thumbprint is refused. Any key a relay cannot
// use is an error rather than skipped: the file is operator configuration.
func LoadKeys(path string) (StaticKeys, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("keys: read %s: %w", path, err)
	}
	defer root.Close()
	raw, err := root.ReadFile(filepath.Base(path))
	if err != nil {
		return nil, fmt.Errorf("keys: read %s: %w", path, err)
	}
	return parseKeys(raw)
}

func parseKeys(raw []byte) (StaticKeys, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("keys: decode: %w", err)
	}
	if len(set.Keys) == 0 {
		return nil, errNoKeys
	}
	keys := make(StaticKeys, len(set.Keys))
	for i, k := range set.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" {
			return nil, fmt.Errorf("keys: key %d: want kty OKP and crv Ed25519, got %q/%q", i, k.Kty, k.Crv)
		}
		key, err := NewKey(k.Kid, k.X)
		if err != nil {
			return nil, fmt.Errorf("keys: key %d: %w", i, err)
		}
		keys[key.ID] = key.Public
	}
	return keys, nil
}

// thumbprint is the RFC 7638 JWK thumbprint of an Ed25519 key: SHA-256 over
// the required members in lexicographic order, base64url without padding.
func thumbprint(x string) string {
	sum := sha256.Sum256([]byte(`{"crv":"Ed25519","kty":"OKP","x":"` + x + `"}`))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
