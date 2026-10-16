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
4. **The relay** verifies the token itself against the key set (`QUMO_AUTH_KEYS=keys.json qumo relay`) and enforces what it grants. The token's expiry decides whether a session may start; the session then lives on until it closes or its key leaves the set ([configuration](../../configuration/#credential-expiry-and-live-sessions)).

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
qumo auth token [-key signing-key.jwk] [-scope ACTIONS:BROADCAST ...] [-publish PATH] [-subscribe PATH] [-sub SUBJECT] [-ttl 1h]
```

Signs a token granting the scopes `-scope` names, valid for `-ttl`, at most one hour. `-scope` may be repeated: `ACTIONS` is a comma-separated list of `publish`, `subscribe`, `fetch`, `create`, `post` and `redact`; `BROADCAST` is a path, or `a/b/**` for every path beneath it (not `a/b` itself). `-publish PATH` is short for `-scope publish:PATH -scope publish:PATH/**`, and `-subscribe PATH` for the same with `subscribe,fetch`; they add to any `-scope`. With no scope at all, the token names its key's tenant alone, as a [relay session's credential](#tickets-on-the-relay) may. `-sub` names the bearer. For example, `qumo auth token -sub alice -scope post:room/123/comments -scope fetch:room/123/**`. The token alone goes to stdout, so `T=$(qumo auth token …)` captures it; what it grants and when it expires go to stderr. A path outside the key's prefix is refused here, as the relay would refuse the token; `token.Sign` does the same.

## What a token grants

A token grants **`scopes`**: actions on broadcasts, each on every track of them, the JSON counterpart of the `moqt` claim of CAT-4-MOQT ([draft-ietf-moq-c4m](https://datatracker.ietf.org/doc/draft-ietf-moq-c4m/)). Each scope lists:

- **`actions`**: `publish` (sending a broadcast's tracks through a relay), `subscribe` (live), `fetch` (history: a funnel's `GET`), `post` (sending one message into a track that exists at a funnel, which gathers many senders into it and publishes it; a relay never reads it as publishing), `redact` (taking a record out of a funnel's track; for credentials the app keeps to itself) and `create` (creating a track at a [funnel](../funnel/#credentials), which the funnel then publishes; for the app, which decides which tracks a broadcast has). Any other action refuses the token.
- **`broadcast`**: `{"exact": "a/b"}` for that path alone, or `{"prefix": "a/b"}` for every path beneath it on `/` boundaries (`a/b/c`, not `a/b` itself nor `a/bc`), as a MoQ path pattern ending in `/` matches. To reach a path and what lies beneath it, grant both (`token.PathAndBeneath`). Either must lie within the key's prefix, and a path may not hold `*`, `{` or `}`, which a relay reads as a pattern.

A token may grant no scope at all (`"scopes": []`). Whatever no scope grants is denied, and a scope member the verifier doesn't know refuses the token, such as a `track` match: a member meant to narrow a scope must not be ignored. A token may name its bearer in **`sub`**, which a funnel records as the sender; without `sub` it names no one.

```json
{"sub": "alice",
 "scopes": [{"actions": ["post"], "broadcast": {"exact": "acme/app/rooms/42/comments"}}],
 "iat": 1791370000, "nbf": 1791370000, "exp": 1791370600, "jti": "…"}
```

**`path_auth` tokens are accepted too**, as other MoQ implementations sign them (the `@moq/token` convention), and read as the scopes they amount to: `pub` as `publish`, and `sub` as `subscribe` and `fetch`, each on its path and every path beneath it, on every track. So

```json
{"path_auth": {"root": "acme/app/rooms/42", "pub": "alice", "sub": ""}, "iat": …, "nbf": …, "exp": …}
```

grants what `token.PathAndBeneath("acme/app/rooms/42/alice", publish)` and `token.PathAndBeneath("acme/app/rooms/42", subscribe, fetch)` do: an exact and a prefix scope each. A token carrying both `path_auth` and `scopes` is refused, and so is one carrying `path_auth` and `sub`. `qumo auth token` and `token.Sign` write `scopes` only.

**On the relay**, a session may announce a broadcast a `publish` scope matches, and the relay takes its tracks from it. A broadcast has one publisher, so give each publisher its own broadcast path; to write into a track a [funnel](../funnel/) publishes, grant `post` there instead. The relay ignores `post` and `create`. A session may subscribe to, request the track info of and fetch from any track of a broadcast a `subscribe` scope matches. The relay ignores `fetch`, so a credential granting only `fetch` (a funnel reader's) may do nothing there.

**A session may list announcements** within one directory per credential: the deepest one its `publish` and `subscribe` scopes share, its *root*. A prefix scope counts as its directory (`room/42` as `/room/42/`), an exact scope as the directory it lies in (`room/42/7` as `/room/42/`). A credential subscribing to `room/42` and publishing `room/42/7` lists `/room/42/` and what lies beneath it, and nothing of another room. Scopes in unrelated trees share only `/`, which lists everything, so sign one credential per room. The root never lies above the key's prefix, so a session lists nothing of another tenant: a scope on the key's prefix itself, which would need it, reaches nothing on the relay.

### Tickets on the relay

A session holds more than the credential it connected with: a client presents further tokens, *tickets*, on MoQ Auth Streams, and the relay adds what each grants to the session's while it is in force. An app can then keep one long-lived session per client, connected with a token of **no scopes**, which names the app's key alone, and grant each room as the client joins it, with a ticket per room. A ticket:

- must be signed by **the same key** as the session's credential, so everything a session may do is one tenant's;
- is verified as a credential is at connect, and a key paused with `"pause": ["publish"]` refuses one that may publish;
- **expires** at its `exp` (with the 60 s leeway), unlike a session's credential: its grant is revoked with `AUTH_ERROR` ("ticket expired"), and the client presents a fresh one to keep it. A key that leaves the set revokes it too, at the next 30 s check;
- is withdrawn by the client closing its Auth Stream;
- is a bearer token, as a credential is: any session of the same key's tenant that holds it may present it.

A session holds at most 32 tickets at once. A relay with auth off, or a session admitted without a credential (a relay peer), takes none: it answers `NOT_SUPPORTED`.

## Signing in your app (Go)

```go
key, err := token.LoadSigningKey("signing-key.jwk") // once, at start
// per client, after your app has decided what this user may do:
tok, err := token.Sign(key, token.Grant{
	Subject: "alice",
	Scopes: []token.Scope{
		// publish every track of the broadcast acme/app/rooms/42/alice
		{Actions: []token.Action{token.ActionPublish}, Broadcast: "acme/app/rooms/42/alice"},
		// watch and read the history of every broadcast in the room
		{Actions: []token.Action{token.ActionSubscribe, token.ActionFetch}, Broadcast: "acme/app/rooms/42", Prefix: true},
		// post into the tracks of its comments broadcast at a funnel, as alice
		{Actions: []token.Action{token.ActionPost}, Broadcast: "acme/app/rooms/42/comments"},
	},
}, time.Hour)
```
