// Package authserver is the auth server a relay asks about every client
// session (qumo auth; qumo-deploy ADR 0035, Decision 3). It verifies the
// app-signed capability token the client presented and turns its grant into
// the publish and subscribe patterns the relay enforces.
//
// A token is an EdDSA JWT an app signs with a key it registered. Its
// path_auth claims are the capabilities: what the bearer may publish and
// subscribe to. qumo never sees the app's users; it checks only that a trusted
// key signed the token, that the token is current, and that every path it
// grants lies within that key's prefix.
package authserver

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Key is a public signing key the auth server trusts.
type Key struct {
	// ID is the key's RFC 7638 JWK thumbprint, the kid qumo assigns at
	// registration and tokens carry in their header.
	ID     string
	Public ed25519.PublicKey
	// Prefix confines every path a token signed by this key may grant, at a
	// "/" boundary. Empty means unconstrained (a static key with no prefix).
	Prefix string
}

var errNoKeys = errors.New("authserver: the key set lists no keys")

// jwk is one Ed25519 public key in JWK form (RFC 8037), plus the qumo
// "prefix" member.
type jwk struct {
	Kty    string `json:"kty"`
	Crv    string `json:"crv"`
	X      string `json:"x"`
	Kid    string `json:"kid,omitempty"`
	Prefix string `json:"prefix,omitempty"`
}

// LoadKeys reads a JWK Set of Ed25519 public keys from path. A key without a
// kid gets its RFC 7638 thumbprint; a key whose kid differs from it is
// refused. Any key that can't be used is an error, not skipped: the file is
// operator configuration.
func LoadKeys(path string) (map[string]Key, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("authserver: read keys: %w", err)
	}
	defer func() { _ = root.Close() }()
	raw, err := root.ReadFile(filepath.Base(path))
	if err != nil {
		return nil, fmt.Errorf("authserver: read keys: %w", err)
	}
	return parseKeys(raw)
}

func parseKeys(raw []byte) (map[string]Key, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("authserver: decode keys: %w", err)
	}
	if len(set.Keys) == 0 {
		return nil, errNoKeys
	}
	keys := make(map[string]Key, len(set.Keys))
	for i, k := range set.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" {
			return nil, fmt.Errorf("authserver: key %d: want kty OKP and crv Ed25519, got %q/%q", i, k.Kty, k.Crv)
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("authserver: key %d: x is not a base64url Ed25519 public key", i)
		}
		kid := thumbprint(x)
		if k.Kid != "" && k.Kid != kid {
			return nil, fmt.Errorf("authserver: key %d: kid %q is not the key's RFC 7638 thumbprint %q", i, k.Kid, kid)
		}
		prefix, err := normalizePath(k.Prefix)
		if err != nil {
			return nil, fmt.Errorf("authserver: key %d: prefix: %w", i, err)
		}
		keys[kid] = Key{ID: kid, Public: ed25519.PublicKey(x), Prefix: prefix}
	}
	return keys, nil
}

// thumbprint is the RFC 7638 JWK thumbprint of an Ed25519 public key: SHA-256
// over the required members in lexicographic order, base64url without
// padding. It encodes x itself, so a valid but non-canonical encoding in the
// key file still yields the kid qumo assigns.
func thumbprint(x []byte) string {
	enc := base64.RawURLEncoding.EncodeToString(x)
	sum := sha256.Sum256([]byte(`{"crv":"Ed25519","kty":"OKP","x":"` + enc + `"}`))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
