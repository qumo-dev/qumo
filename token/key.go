package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Key is a public signing key a verifier trusts.
type Key struct {
	// ID is the key's RFC 7638 JWK thumbprint, the kid a token carries in
	// its header.
	ID     string
	Public ed25519.PublicKey
	// Prefix confines every path a token signed by this key may grant, at a
	// "/" boundary. Empty leaves the key unconstrained.
	Prefix string
}

// SigningKey is an app's private signing key. Keep it on the app's server;
// only its public half (Public) goes to the auth server.
type SigningKey struct {
	// ID is the key's RFC 7638 JWK thumbprint, set in every token's header.
	ID      string
	Private ed25519.PrivateKey
	// Prefix is the prefix the auth server confines this key to; Sign
	// refuses a grant outside it.
	Prefix string
}

var errNoKeys = errors.New("token: the key set lists no keys")

// GenerateKey returns a new Ed25519 signing key, confined to prefix ("" for
// none).
func GenerateKey(prefix string) (SigningKey, error) {
	norm, err := normalizePath(prefix)
	if err != nil {
		return SigningKey{}, fmt.Errorf("token: prefix: %w", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return SigningKey{}, fmt.Errorf("token: generate key: %w", err)
	}
	return SigningKey{ID: thumbprint(pub), Private: priv, Prefix: norm}, nil
}

// Public returns the Key a verifier trusts for k.
func (k SigningKey) Public() Key {
	return Key{ID: k.ID, Public: k.Private.Public().(ed25519.PublicKey), Prefix: k.Prefix}
}

// jwk is one Ed25519 key in JWK form (RFC 8037), plus a "prefix" member. D is
// set only for a private key.
type jwk struct {
	Kty    string `json:"kty"`
	Crv    string `json:"crv"`
	X      string `json:"x"`
	D      string `json:"d,omitempty"`
	Kid    string `json:"kid,omitempty"`
	Prefix string `json:"prefix,omitempty"`
}

func (k Key) jwk() jwk {
	return jwk{Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(k.Public), Kid: k.ID, Prefix: k.Prefix}
}

// MarshalKeySet encodes keys as a JWK Set, the file an auth server loads
// (LoadKeySet).
func MarshalKeySet(keys ...Key) ([]byte, error) {
	set := struct {
		Keys []jwk `json:"keys"`
	}{Keys: make([]jwk, 0, len(keys))}
	for _, k := range keys {
		set.Keys = append(set.Keys, k.jwk())
	}
	return json.Marshal(set, jsontext.WithIndent("  "))
}

// MarshalJWK encodes k as a private JWK, the file an app signs with
// (LoadSigningKey).
func (k SigningKey) MarshalJWK() ([]byte, error) {
	j := k.Public().jwk()
	j.D = base64.RawURLEncoding.EncodeToString(k.Private.Seed())
	return json.Marshal(j, jsontext.WithIndent("  "))
}

// ParseSigningKey decodes a private Ed25519 JWK.
func ParseSigningKey(raw []byte) (SigningKey, error) {
	var j jwk
	if err := json.Unmarshal(raw, &j); err != nil {
		return SigningKey{}, fmt.Errorf("token: decode signing key: %w", err)
	}
	if j.Kty != "OKP" || j.Crv != "Ed25519" {
		return SigningKey{}, fmt.Errorf("token: signing key: want kty OKP and crv Ed25519, got %q/%q", j.Kty, j.Crv)
	}
	seed, err := base64.RawURLEncoding.DecodeString(j.D)
	if err != nil || len(seed) != ed25519.SeedSize {
		return SigningKey{}, errors.New("token: signing key: d is not a base64url Ed25519 private key")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	kid := thumbprint(priv.Public().(ed25519.PublicKey))
	if j.Kid != "" && j.Kid != kid {
		return SigningKey{}, fmt.Errorf("token: signing key: kid %q is not the key's RFC 7638 thumbprint %q", j.Kid, kid)
	}
	prefix, err := normalizePath(j.Prefix)
	if err != nil {
		return SigningKey{}, fmt.Errorf("token: signing key: prefix: %w", err)
	}
	return SigningKey{ID: kid, Private: priv, Prefix: prefix}, nil
}

// LoadSigningKey reads a private Ed25519 JWK from path.
func LoadSigningKey(path string) (SigningKey, error) {
	raw, err := readFile(path)
	if err != nil {
		return SigningKey{}, err
	}
	return ParseSigningKey(raw)
}

// ParseKeySet decodes a JWK Set of Ed25519 public keys, by kid. A key without
// a kid gets its RFC 7638 thumbprint; a key whose kid differs from it, or that
// is listed twice, is refused. Any key that can't be used is an error, not skipped: the set is
// configuration.
func ParseKeySet(raw []byte) (map[string]Key, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("token: decode key set: %w", err)
	}
	if len(set.Keys) == 0 {
		return nil, errNoKeys
	}
	keys := make(map[string]Key, len(set.Keys))
	for i, k := range set.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" {
			return nil, fmt.Errorf("token: key %d: want kty OKP and crv Ed25519, got %q/%q", i, k.Kty, k.Crv)
		}
		if k.D != "" {
			return nil, fmt.Errorf("token: key %d: a key set holds public keys only, but this one has a private part (d)", i)
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("token: key %d: x is not a base64url Ed25519 public key", i)
		}
		kid := thumbprint(x)
		if k.Kid != "" && k.Kid != kid {
			return nil, fmt.Errorf("token: key %d: kid %q is not the key's RFC 7638 thumbprint %q", i, k.Kid, kid)
		}
		if _, dup := keys[kid]; dup {
			// Two entries could give one key two prefixes; neither may win.
			return nil, fmt.Errorf("token: key %d: key %q is listed twice", i, kid)
		}
		prefix, err := normalizePath(k.Prefix)
		if err != nil {
			return nil, fmt.Errorf("token: key %d: prefix: %w", i, err)
		}
		keys[kid] = Key{ID: kid, Public: ed25519.PublicKey(x), Prefix: prefix}
	}
	return keys, nil
}

// LoadKeySet reads a JWK Set of Ed25519 public keys from path (ParseKeySet).
func LoadKeySet(path string) (map[string]Key, error) {
	raw, err := readFile(path)
	if err != nil {
		return nil, err
	}
	return ParseKeySet(raw)
}

// readFile reads path through an os.Root on its directory, so the read can't
// leave that directory.
func readFile(path string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("token: read %s: %w", path, err)
	}
	defer func() { _ = root.Close() }() // not actionable: a read-only handle
	raw, err := root.ReadFile(filepath.Base(path))
	if err != nil {
		return nil, fmt.Errorf("token: read %s: %w", path, err)
	}
	return raw, nil
}

// thumbprint is the RFC 7638 JWK thumbprint of an Ed25519 public key: SHA-256
// over the required members in lexicographic order, base64url without
// padding. It encodes x itself, so a valid but non-canonical encoding of the
// key still yields the same kid.
func thumbprint(x []byte) string {
	enc := base64.RawURLEncoding.EncodeToString(x)
	sum := sha256.Sum256([]byte(`{"crv":"Ed25519","kty":"OKP","x":"` + enc + `"}`))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
