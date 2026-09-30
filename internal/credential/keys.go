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

// StaticKeys is a fixed key set by kid: the public keys a relay operator lists
// in the relay's own configuration. It never changes while the relay runs.
type StaticKeys map[string]ed25519.PublicKey

var _ Keys = StaticKeys(nil)

// Key returns the public key for kid. An unknown kid is refused, with no
// lookup elsewhere.
func (k StaticKeys) Key(_ context.Context, kid string) (ed25519.PublicKey, error) {
	pub, ok := k[kid]
	if !ok {
		return nil, errUnknownKey
	}
	return pub, nil
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
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("keys: key %d: x is not a base64url Ed25519 public key", i)
		}
		kid := thumbprint(k.X)
		if k.Kid != "" && k.Kid != kid {
			return nil, fmt.Errorf("keys: key %d: kid %q is not the key's RFC 7638 thumbprint %q", i, k.Kid, kid)
		}
		keys[kid] = ed25519.PublicKey(x)
	}
	return keys, nil
}

// thumbprint is the RFC 7638 JWK thumbprint of an Ed25519 key: SHA-256 over
// the required members in lexicographic order, base64url without padding.
func thumbprint(x string) string {
	sum := sha256.Sum256([]byte(`{"crv":"Ed25519","kty":"OKP","x":"` + x + `"}`))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
