---
title: auth
description: Generate signing keys and sign test tokens.
weight: 2
---

`qumo auth` sets up session auth for a relay ([configuration](../../configuration/#session-auth-optional)). Your app keeps the only decision that is really yours: **who may publish or watch what**, which it states by what it signs. The key format, signing, verification and path confinement are done for you.

**How it fits together:**
1. **Once:** generate a signing key pair (`qumo auth keygen`). The private key stays on your app's server; the relay gets the public one, in a key set.
2. **Per client:** your server signs a token naming what that client may do on which broadcasts and tracks ([below](#what-a-token-grants)). Use the Go package `github.com/qumo-dev/qumo/token`, or `qumo auth token` while testing.
3. **The client** connects with the token in the relay URL: `https://relay:4433/?jwt=<token>` (WebTransport), or `moqt://relay:4433/?jwt=<token>` (native QUIC).
4. **The relay** verifies the token itself against the key set (`QUMO_AUTH_KEYS=keys.json qumo relay`) and enforces what it grants. The token's expiry decides whether a session may start; the session then lives on, unless the token carries `reval`, in which case it ends when the token expires and the client reconnects with a fresh one ([configuration](../../configuration/#credential-expiry-and-live-sessions)).

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
qumo auth token [-key signing-key.jwk] [-scope ACTIONS:BROADCAST[:TRACK] ...] [-publish PATH] [-subscribe PATH] [-sub SUBJECT] [-ttl 1h] [-reval 1m]
```

Signs a token granting the scopes `-scope` names, valid for `-ttl`, at most one hour. `-scope` may be repeated: `ACTIONS` is a comma-separated list of `publish`, `subscribe` and `fetch`; `BROADCAST` is a path, or `a/b/**` for it and every path beneath it; `TRACK` is one track name, or omitted for every track. `-publish PATH` is short for `-scope publish:PATH/**`, and `-subscribe PATH` for `-scope subscribe,fetch:PATH/**`; they add to any `-scope`. At least one scope is needed. `-sub` names the bearer. With `-reval`, between 30 s and one hour, the token carries `reval` and a session it admits ends when it expires; without it, the session outlives the token. For example, `qumo auth token -sub alice -scope publish:room/123:chat -scope fetch:room/123/**`. The token alone goes to stdout, so `T=$(qumo auth token …)` captures it; what it grants and when it expires go to stderr. A path outside the key's prefix is refused here, as the relay would refuse the token; `token.Sign` does the same.

## What a token grants

A token grants **`scopes`**: actions on broadcasts and tracks, the JSON counterpart of the `moqt` claim of CAT-4-MOQT ([draft-ietf-moq-c4m](https://datatracker.ietf.org/doc/draft-ietf-moq-c4m/)). Each scope lists:

- **`actions`**: `publish` (a track's groups: publishing through a relay, recording at a [funnel](../funnel/#credentials)), `subscribe` (live) and `fetch` (history: a funnel's `GET`). Any other action refuses the token.
- **`broadcast`**: `{"exact": "a/b"}` for that path alone, or `{"prefix": "a/b"}` for it and every path beneath it on `/` boundaries (not `a/bc`). Either must lie within the key's prefix.
- **`track`** (optional): `{"exact": "chat"}` for that track alone; omitted, every track.

Whatever no scope grants is denied, and a scope member the verifier doesn't know refuses the token, so a misspelled `track` never widens a scope to every track. A token may name its bearer in **`sub`**, which a funnel records as the sender; without `sub` it names no one.

```json
{"sub": "alice",
 "scopes": [{"actions": ["publish"], "broadcast": {"exact": "acme/app/rooms/42"}, "track": {"exact": "chat"}}],
 "iat": 1791370000, "nbf": 1791370000, "exp": 1791370600, "jti": "…"}
```

**`path_auth` tokens are accepted too**, as other MoQ implementations sign them (the `@moq/token` convention), and read as the scopes they amount to: `pub` as a `publish` scope, and `sub` as a `subscribe` and `fetch` scope, each on its path and every path beneath it, on every track. So

```json
{"path_auth": {"root": "acme/app/rooms/42", "pub": "alice", "sub": ""}, "iat": …, "nbf": …, "exp": …}
```

grants what `[{"actions": ["publish"], "broadcast": {"prefix": "acme/app/rooms/42/alice"}}, {"actions": ["subscribe", "fetch"], "broadcast": {"prefix": "acme/app/rooms/42"}}]` does. A token carrying both `path_auth` and `scopes` is refused, and so is one carrying `path_auth` and `sub`. `qumo auth token` and `token.Sign` write `scopes` only.

**On the relay**, a session may announce a broadcast a `publish` scope matches, whatever track that scope names; the relay takes a track of it from the publisher only when a `publish` scope matches the broadcast and the track, and answers a subscriber asking for any other track as if it did not exist. A session may subscribe to a track a `subscribe` scope matches, by broadcast and track name. The relay ignores `fetch`, so a credential granting only `fetch` (a funnel reader's) may do nothing there.

## Signing in your app (Go)

```go
key, err := token.LoadSigningKey("signing-key.jwk") // once, at start
// per client, after your app has decided what this user may do:
tok, err := token.Sign(key, token.Grant{
	Subject: "alice",
	Scopes: []token.Scope{
		// publish every track of the broadcasts at and beneath acme/app/rooms/42/alice
		{Actions: []token.Action{token.ActionPublish}, Broadcast: "acme/app/rooms/42/alice", Prefix: true},
		// watch and read the history of the whole room
		{Actions: []token.Action{token.ActionSubscribe, token.ActionFetch}, Broadcast: "acme/app/rooms/42", Prefix: true},
		// record into its chat track at a funnel, as alice
		{Actions: []token.Action{token.ActionPublish}, Broadcast: "acme/app/rooms/42", Track: "chat"},
	},
}, time.Hour)

// or, for a session that must end when the token expires:
tok, err = token.Sign(key, token.Grant{Scopes: []token.Scope{
	{Actions: []token.Action{token.ActionSubscribe}, Broadcast: "acme/app/rooms/42", Prefix: true},
}}, 5*time.Minute, token.WithReval(time.Minute))
```
