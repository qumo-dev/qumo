---
title: auth
description: Generate signing keys and sign test tokens.
weight: 2
---

`qumo auth` sets up session auth for a relay ([configuration](../../configuration/#session-auth-optional)). Your app keeps the only decision that is really yours: **who may publish or watch what**, which it states by what it signs. The key format, signing, verification and path confinement are done for you.

**How it fits together:**
1. **Once:** generate a signing key pair (`qumo auth keygen`). The private key stays on your app's server; the relay gets the public one, in a key set.
2. **Per client:** your server signs a token naming the paths that client may publish to or subscribe to, or the exact broadcasts and tracks it may act on ([below](#what-a-token-grants)). Use the Go package `github.com/qumo-dev/qumo/token`, or `qumo auth token` while testing.
3. **The client** connects with the token in the relay URL: `https://relay:4433/?jwt=<token>` (WebTransport), or `moqt://relay:4433/?jwt=<token>` (native QUIC).
4. **The relay** verifies the token itself against the key set (`QUMO_AUTH_KEYS=keys.json qumo relay`) and enforces what it grants. A session ends when its token expires; the client reconnects with a fresh one.

## Usage

```
qumo auth <command> [flags]
```

| Command | Description |
|---|---|
| `keygen` | Generate a signing key pair: the private key, and the public key set the relay trusts. |
| `token` | Sign a token by hand, for testing. |

### keygen

```
qumo auth keygen [-prefix acme/app] [-out signing-key.jwk] [-keys keys.json]
```

| Flag | Default | Description |
|---|---|---|
| `-prefix` | (none) | Confine the key: every path a token signed with it may grant must lie at or beneath this prefix. |
| `-out` | `signing-key.jwk` | The private signing key (an Ed25519 JWK). Keep it on your app's server. |
| `-keys` | `keys.json` | The public key set (a JWK Set) for `QUMO_AUTH_KEYS`. |

It prints the key's `kid` (its RFC 7638 thumbprint), the paths it may grant, and the next steps. It refuses to overwrite an existing file, since replacing a key would invalidate every token signed with it; if it can't write the key set, it removes the private key it just wrote.

### token

```
qumo auth token [-key signing-key.jwk] [-publish PATH] [-subscribe PATH] [-ttl 1h]
qumo auth token [-key signing-key.jwk] -scope ACTIONS:BROADCAST[:TRACK] ... [-sub SUBJECT] [-ttl 1h]
```

Signs a token granting publish at or beneath `-publish` and subscribe at or beneath `-subscribe` (either may be omitted, not both), or instead the scopes `-scope` names, valid for `-ttl`, at most one hour. `-scope` may be repeated: `ACTIONS` is a comma-separated list of `publish`, `subscribe`, `fetch` and `announce`; `BROADCAST` is a path, or `a/b/**` for it and every path beneath it; `TRACK` is one track name, or omitted for every track. `-sub` names the bearer, with `-scope` only. For example, `qumo auth token -sub alice -scope publish:room/123:chat -scope fetch:room/123/**`. The token alone goes to stdout, so `T=$(qumo auth token …)` captures it; what it grants and when it expires go to stderr. A path outside the key's prefix is refused here, as the relay would refuse the token; `token.Sign` does the same.

## What a token grants

A token grants in one of two forms, never both: a token carrying both is refused.

**`path_auth`** grants paths: one the bearer may publish at or beneath, and one it may subscribe at or beneath.

```json
{"path_auth": {"root": "acme/app/rooms/42", "pub": "alice", "sub": ""}, "iat": …, "nbf": …, "exp": …}
```

**`scopes`** grants actions on exact broadcasts and tracks, the JSON counterpart of the `moqt` claim of CAT-4-MOQT ([draft-ietf-moq-c4m](https://datatracker.ietf.org/doc/draft-ietf-moq-c4m/)). Each scope lists:

- **`actions`**: `publish` (a track's groups: publishing through a relay, recording at a [funnel](../funnel/#credentials)), `subscribe` (live), `fetch` (history: a funnel's `GET`) and `announce` (a broadcast). Any other action refuses the token.
- **`broadcast`**: `{"exact": "a/b"}` for that path alone, or `{"prefix": "a/b"}` for it and every path beneath it on `/` boundaries (not `a/bc`). Either must lie within the key's prefix.
- **`track`** (optional): `{"exact": "chat"}` for that track alone; omitted, every track. An action on a broadcast as a whole, such as `announce`, is matched only by a scope with no `track`.

Whatever no scope grants is denied, and a scope member the verifier doesn't know refuses the token, so a misspelled `track` never widens a scope to every track. A token with `scopes` may name its bearer in **`sub`**, which a funnel records as the sender.

```json
{"sub": "alice",
 "scopes": [{"actions": ["publish"], "broadcast": {"exact": "acme/app/rooms/42"}, "track": {"exact": "chat"}}],
 "iat": 1791370000, "nbf": 1791370000, "exp": 1791370600, "jti": "…"}
```

The relay admits a session with a `scopes` credential, but does not yet enforce scopes: on the relay such a session may publish and subscribe nothing. Use `path_auth` for relay sessions, and `scopes` for a [funnel](../funnel/#credentials).

## Signing in your app (Go)

```go
key, err := token.LoadSigningKey("signing-key.jwk") // once, at start
// per client, after your app has decided what this user may do:
tok, err := token.Sign(key, token.Grant{
	Publish:   "acme/app/rooms/42/alice",
	Subscribe: "acme/app/rooms/42",
}, time.Hour)
// or, for exact tracks and a named bearer:
tok, err = token.Sign(key, token.Grant{
	Subject: "alice",
	Scopes: []token.Scope{
		{Actions: []token.Action{token.ActionPublish}, Broadcast: "acme/app/rooms/42", Track: "chat"},
		{Actions: []token.Action{token.ActionFetch}, Broadcast: "acme/app/rooms/42", Prefix: true},
	},
}, time.Hour)
```
