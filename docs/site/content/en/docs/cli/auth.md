---
title: auth
description: Generate signing keys, sign test tokens, and run an auth server.
weight: 2
---

`qumo auth` sets up session auth for a relay ([configuration](../../configuration/#session-auth-optional)). Your app keeps the only decision that is really yours: **who may publish or watch what**, which it states by what it signs. The key format, signing, verification and path confinement are done for you.

**How it fits together:**
1. **Once:** generate a signing key pair (`qumo auth keygen`). The private key stays on your app's server; the relay gets the public one, in a key set.
2. **Per client:** your server signs a token naming the paths that client may publish to or subscribe to. Use the Go package `github.com/qumo-dev/qumo/token`, or `qumo auth token` while testing.
3. **The client** connects with the token in the relay URL: `https://relay:4433/?jwt=<token>` (WebTransport), or `moqt://relay:4433/?jwt=<token>` (native QUIC).
4. **The relay** verifies the token itself against the key set (`QUMO_AUTH_KEYS_FILE=keys.json qumo relay`) and enforces what it grants. A session ends when its token expires; the client reconnects with a fresh one.

Running `qumo auth` as a server, which the relay then asks (`QUMO_AUTH_URL`), is the alternative to step 4, for when verification should run outside the relay process.

## Usage

```
qumo auth [command] [flags]
```

With no command, `qumo auth` runs the auth server, as `qumo relay` runs the relay. Most setups don't need it: the relay verifies against the key set itself.

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
| `-keys` | `keys.json` | The public key set (a JWK Set) for the relay's `QUMO_AUTH_KEYS_FILE`. |

It prints the key's `kid` (its RFC 7638 thumbprint), the paths it may grant, and the next steps. It refuses to overwrite an existing file, since replacing a key would invalidate every token signed with it; if it can't write the key set, it removes the private key it just wrote.

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

A token it can't accept, or none, gets 401; a valid one that grants a path outside its key's prefix gets 403. A key set that lists a key twice is refused at startup.

At startup it prints its address, the `QUMO_AUTH_URL` for the relay, and each trusted key with the paths it may grant. It logs every admitted session with its grant, every refusal with its reason, and every session end with its duration and bytes. The token is never logged.

### token

```
qumo auth token [-key signing-key.jwk] [-publish PATH] [-subscribe PATH] [-ttl 1h]
```

Signs a token granting publish at or beneath `-publish` and subscribe at or beneath `-subscribe` (either may be omitted, not both), valid for `-ttl`, at most one hour. The token alone goes to stdout, so `T=$(qumo auth token …)` captures it; what it grants and when it expires go to stderr. A path outside the key's prefix is refused here, as the server would refuse the token; `token.Sign` does the same.

## Signing in your app (Go)

```go
key, err := token.LoadSigningKey("signing-key.jwk") // once, at start
// per client, after your app has decided what this user may do:
tok, err := token.Sign(key, token.Grant{
	Publish:   "acme/app/rooms/42/alice",
	Subscribe: "acme/app/rooms/42",
}, time.Hour)
```
