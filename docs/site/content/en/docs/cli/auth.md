---
title: auth
description: Generate signing keys and sign test tokens.
weight: 2
---

`qumo auth` sets up session auth for a relay ([configuration](../../configuration/#session-auth-optional)). Your app keeps the only decision that is really yours: **who may publish or watch what**, which it states by what it signs. The key format, signing, verification and path confinement are done for you.

**How it fits together:**
1. **Once:** generate a signing key pair (`qumo auth keygen`). The private key stays on your app's server; the relay gets the public one, in a key set.
2. **Per client:** your server signs a token naming what that client may do on which broadcasts ([below](#what-a-token-grants)). Use the Go package `github.com/qumo-dev/qumo/token`, or `qumo auth token` while testing.
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
| `-prefix` | (none) | Confine the key: the root of every token signed with it must lie at or beneath this prefix, and so every path it grants. |
| `-out` | `signing-key.jwk` | The private signing key (an Ed25519 JWK). Keep it on your app's server. |
| `-keys` | `keys.json` | The public key set (a JWK Set) for `QUMO_AUTH_KEYS`. |

It prints the key's `kid` (its RFC 7638 thumbprint), the roots it may sign ("a root at or beneath `acme/app`"), and the next steps. It refuses to overwrite an existing file, since replacing a key would invalidate every token signed with it; if it can't write the key set, it removes the private key it just wrote.

### token

```
qumo auth token [-key signing-key.jwk] [-root PATH] [-publish PATTERN ...] [-subscribe PATTERN ...] [-create PATTERN ...] [-post PATTERN ...] [-redact PATTERN ...] [-sub SUBJECT] [-ttl 1h] [-ticket]
```

Signs a token granting, beneath `-root`, each action on the path patterns its flag names, valid for `-ttl`, at most one hour. Each action's flag may be repeated, and its patterns are relative to the root ([below](#what-a-token-grants)): `/x` is that path, `/x/` every path beneath it, `/` everything beneath the root, and a segment may be a `{name}` wildcard. With no pattern at all, the token names its key's tenant alone, as a [relay session's credential](#tickets-on-the-relay) may. `-sub` names the bearer. For example, `qumo auth token -root room/123 -sub alice -post /comments -subscribe /`. The token alone goes to stdout, so `T=$(qumo auth token …)` captures it; its root, each action's patterns and when it expires go to stderr. A root outside the key's prefix, or a pattern that doesn't parse, is refused here, as the relay would refuse the token; `token.Sign` does the same.

## What a token grants

A token grants what a MoQ grant does: a **`root`**, the directory of broadcast paths it applies to, and for each action the **path patterns** beneath the root its bearer may act on, on every track of each broadcast they match. It is the shape of gomoqt's `moqt.Grant`, so the relay hands the root and the `publish` and `subscribe` patterns to the session unchanged.

- **`root`**: a broadcast path such as `acme/app/rooms/42` (surrounding slashes are trimmed). It must lie at or beneath the key's prefix, and may not hold a `.` or `..` segment, `*`, `{` or `}`. Every pattern lies beneath it, so it confines what the token grants to the key's tenant. The root is a directory, and its own path is not a broadcast: no pattern reaches it, so give each broadcast a path beneath a root, as a file lies in a directory. A key confined to `acme/app` can't grant the broadcast `/acme/app` itself.
- **`publish`**, **`subscribe`**, **`create`**, **`post`**, **`redact`**: each a list of patterns relative to the root, matching broadcast paths as `moqt.Grant` reads them, the way an `http.ServeMux` pattern matches URL paths. With the root `acme/app/rooms/42`, `/7` matches `/acme/app/rooms/42/7` alone, `/7/` every path beneath it, and `/` every path beneath the root (not the root itself). A segment may be a wildcard: `{name}` matches any one segment (`/{user}/audio`), `{name...}` as the last segment the rest of the path, and `{$}` as the last segment only the `/` before it. A pattern starts with `/` and may not hold a `.` or `..` segment; one that doesn't parse refuses the token.

The actions are `publish` (sending a broadcast's tracks through a relay), `subscribe` (receiving a track, live or as history: a relay's FETCH and a funnel's `GET` read history with it), `post` (sending one message into a track that exists at a funnel, which gathers many senders into it and publishes it; a relay never reads it as publishing), `redact` (taking a record out of a funnel's track; for credentials the app keeps to itself) and `create` (creating a track at a [funnel](../funnel/#credentials), which the funnel then publishes; for the app, which decides which tracks a broadcast has).

A token may grant no pattern at all, and may then leave out `root`. Whatever no pattern grants is denied, and a claim the verifier doesn't know refuses the token, so an action spelled wrong, or the `scopes` claim of earlier releases, never passes as granting nothing more. A token may name its bearer in **`sub`**, which a funnel records as the sender; without `sub` it names no one.

```json
{"root": "acme/app/rooms/42", "sub": "alice",
 "publish": ["/alice"], "subscribe": ["/"], "post": ["/comments"],
 "iat": 1791370000, "nbf": 1791370000, "exp": 1791370600, "jti": "…"}
```

**`path_auth` tokens are accepted too**, as other MoQ implementations sign them (the `@moq/token` convention), and read as the grant they amount to: their `root` as the root, `pub` as `publish`, and `sub` as `subscribe`, each on its path and every path beneath it, on every track; an empty `pub` or `sub` grants what lies beneath the root, not the root's own path. So

```json
{"path_auth": {"root": "acme/app/rooms/42", "pub": "alice", "sub": ""}, "iat": …, "nbf": …, "exp": …}
```

grants what `{"root": "acme/app/rooms/42", "publish": ["/alice", "/alice/"], "subscribe": ["/"]}` does. A token carrying both `path_auth` and `root` or a pattern is refused, and so is one carrying `path_auth` and `sub`. `qumo auth token` and `token.Sign` never write `path_auth`.

**`subscribe` is both live and history**, so a credential that reads a funnel track's history may also subscribe to the broadcasts it names on a relay, and one that listens live may read their history: grant patterns as narrow as the bearer needs.

**On the relay**, a session may announce a broadcast a `publish` pattern matches, and the relay takes its tracks from it. A broadcast has one publisher, so give each publisher its own broadcast path; to write into a track a [funnel](../funnel/) publishes, grant `post` there instead. The relay ignores `post` and `create`. A session may subscribe to, request the track info of and fetch from any track of a broadcast a `subscribe` pattern matches.

**A session may list announcements within its token's root**: a credential whose root is `acme/app/rooms/42` lists `/acme/app/rooms/42/` and what lies beneath it, and nothing of another room. Since the root lies at or beneath the key's prefix, a session lists nothing of another tenant. A credential with no `publish` or `subscribe` pattern grants nothing on the relay, not even listing.

### Tickets on the relay

A session holds more than the credential it connected with: a client presents further tokens, *tickets*, on MoQ Auth Streams, and the relay adds what each grants to the session's while it is in force. An app can then keep one long-lived session per client, connected with a token of **no patterns**, which names the app's key alone, and grant each room as the client joins it, with a ticket per room. A ticket:

- is signed with **`token.SignTicket`** (or `qumo auth token -ticket`), which marks it with the header `typ: "ticket+jwt"` (RFC 8725's explicit typing; `application/ticket+jwt` and any case too). The relay refuses a ticket as the credential a session connects with, where its grant would last as long as the session rather than until the ticket's `exp`. The mark is the issuer's to set: a token signed with `token.Sign` connects whatever it grants, so sign every ticket with `SignTicket`;
- must be signed by **the same key** as the session's credential, so everything a session may do is one tenant's;
- is verified as a credential is at connect, and a key paused with `"pause": ["publish"]` refuses one that may publish;
- **expires** at its `exp` (with the 60 s leeway), unlike a session's credential. The `AUTH_OK` granting it says when (its max age), so the client presents a fresh one in time; at expiry its grant is revoked with `AUTH_ERROR` ("grant expired"). A key that leaves the set, or whose prefix no longer holds the ticket's root, revokes it too, at the next 30 s check;
- that could not be checked just now, as while the relay has no usable key set, is answered `AUTH_ERROR` with `INTERNAL` (`0x1`), which the client may retry; a refused one with `UNAUTHORIZED` (`0x2`);
- is withdrawn by the client closing its Auth Stream;
- is a bearer token, as a credential is: any session of the same key's tenant that holds it may present it.

A session holds at most 32 tickets at once. An Auth Stream carrying no token is answered with the session's own grant again, which adds nothing; a session holds at most 32 of those beside its connection's own. A session granted a ticket that may publish is reported as a publisher from then on, for the rest of its life ([usage](../../configuration/#usage-reports)). A relay with auth off, or a session admitted without a credential (a relay peer), takes none: it answers `NOT_SUPPORTED`.

## Signing in your app (Go)

```go
key, err := token.LoadSigningKey("signing-key.jwk") // once, at start
// per client, after your app has decided what this user may do:
tok, err := token.Sign(key, token.Grant{
	Root:    "acme/app/rooms/42", // at or beneath the key's prefix
	Subject: "alice",
	// publish every track of the broadcast acme/app/rooms/42/alice
	Publish: []string{"/alice"},
	// watch and read the history of every broadcast in the room
	Subscribe: []string{"/"},
	// post into the tracks of its comments broadcast at a funnel, as alice
	Post: []string{"/comments"},
}, time.Hour)
```

`token.Grant` also matches a path as the relay and funnel do (`Allows`), and gives the MoQ grant a relay session receives (`MoQ`).
