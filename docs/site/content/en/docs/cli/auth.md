---
title: auth
description: Run the auth server a relay asks, generate signing keys, and sign test tokens.
weight: 2
---

A relay with `QUMO_AUTH_URL` asks an auth server about every client session ([configuration](../../configuration/#session-auth-optional)). `qumo auth` is one, ready to run, with the tools to set it up. Your app keeps the only decision that is really yours: **who may publish or watch what**, which it states by what it signs. The key format, signing, verification and path confinement are done for you.

**How it fits together:**
1. **Once:** generate a signing key pair. The private key stays on your app's server; the auth server gets the public one.
2. **Per client:** your server signs a token naming the paths that client may publish to or subscribe to. Use the Go package `github.com/qumo-dev/qumo/token`, or `qumo auth token` while testing.
3. **The client** connects with the token in the relay URL: `https://relay:4433/?jwt=<token>` (WebTransport), or `moqt://relay:4433/?jwt=<token>` (native QUIC).
4. **The relay** asks `qumo auth`, which verifies the token, and enforces what it grants. A session ends when its token expires; the client reconnects with a fresh one.

## Usage

```
qumo auth [command] [flags]
```

With no command, `qumo auth` runs the auth server, as `qumo relay` runs the relay.

| Command | Description |
|---|---|
| `keygen` | Generate a signing key pair: the private key, and the public key set the auth server trusts. |
| `token` | Sign a token by hand, for testing. |

### keygen

```
qumo auth keygen [-prefix acme/app] [-out signing-key.jwk] [-keys keys.json]
```

| Flag | Default | Description |
|---|---|---|
| `-prefix` | (none) | Confine the key: every path a token signed with it may grant must lie at or beneath this prefix. |
| `-out` | `signing-key.jwk` | The private signing key (an Ed25519 JWK). Keep it on your app's server. |
| `-keys` | `keys.json` | The public key set (a JWK Set) for `QUMO_AUTH_KEYS_FILE`. |

It refuses to overwrite an existing file, since replacing a key would invalidate every token signed with it. The key's `kid` is its RFC 7638 thumbprint.

### The server

Configured by the environment:

| Variable | Default | Description |
|---|---|---|
| `QUMO_AUTH_KEYS_FILE` | (required) | The trusted public keys (`keygen -keys`). Each may carry a `prefix`. |
| `QUMO_AUTH_ADDR` | `127.0.0.1:4440` | Listen address. Loopback: the relay beside it is the only client. |

Point the relay at it with `QUMO_AUTH_URL=http://127.0.0.1:4440`. A session without a token is refused; for a relay open to everyone, leave `QUMO_AUTH_URL` unset instead.

**What it checks, in order:**
1. A trusted `kid`, and `alg` EdDSA.
2. The signature.
3. Exactly the allowed claims: `path_auth`, `iat`, `nbf`, `exp`, and an optional `jti`.
4. The time claims, with 60 s leeway and a lifetime of at most one hour.
5. Every granted path lies within the key's prefix.

A token it can't accept gets 401; a valid one that grants nothing usable gets 403. The token is never logged.

### token

```
qumo auth token [-key signing-key.jwk] [-publish PATH] [-subscribe PATH] [-ttl 1h]
```

Prints a token granting publish at or beneath `-publish` and subscribe at or beneath `-subscribe` (either may be omitted, not both), valid for `-ttl`, at most one hour. A path outside the key's prefix is refused here, as the server would refuse the token; `token.Sign` does the same.

## Signing in your app (Go)

```go
key, err := token.LoadSigningKey("signing-key.jwk") // once, at start
// per client, after your app has decided what this user may do:
tok, err := token.Sign(key, token.Grant{
	Publish:   "acme/app/rooms/42/alice",
	Subscribe: "acme/app/rooms/42",
}, time.Hour)
```
