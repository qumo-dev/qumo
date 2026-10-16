---
title: auth
description: Generate signing keys and sign test tokens.
weight: 2
---

`qumo auth` sets up session auth for a relay ([configuration](../../configuration/#session-auth-optional)). Your app keeps the only decision that is really yours: **who may publish or watch what**, which it states by what it signs. The key format, signing, verification and path confinement are done for you.

**How it fits together:**
1. **Once:** generate a signing key pair (`qumo auth keygen`). The private key stays on your app's server; the relay gets the public one, in a key set.
2. **Per client:** your server signs a token naming what that client may do on which broadcasts ([below](#what-a-token-grants)). Use the Go package `github.com/qumo-dev/qumo/token`, or `qumo auth token` while testing.
3. **The client** connects with the token in the relay URL: `https://relay:4433/?jwt=<token>` (WebTransport), or `moqt://relay:4433/?jwt=<token>` (native QUIC). Or it connects with no token and presents one on the session's Auth Stream ([below](#tickets-on-the-relay)).
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
qumo auth keygen [-prefix /acme/app/] [-out signing-key.jwk] [-keys keys.json]
```

| Flag | Default | Description |
|---|---|---|
| `-prefix` | `/` (none) | Confine the key to a directory: the root of every token signed with it must lie at or beneath it, and so every path it grants. A bare path (`acme/app`) reads as the directory it names (`/acme/app/`), and a key set written with one still loads. |
| `-out` | `signing-key.jwk` | The private signing key (an Ed25519 JWK). Keep it on your app's server. |
| `-keys` | `keys.json` | The public key set (a JWK Set) for `QUMO_AUTH_KEYS`. |

It prints the key's `kid` (its RFC 7638 thumbprint), the roots it may sign ("a root at or beneath `/acme/app/`"), and the next steps. It refuses to overwrite an existing file, since replacing a key would invalidate every token signed with it; if it can't write the key set, it removes the private key it just wrote.

### token

```
qumo auth token [-key signing-key.jwk] [-root DIR] [-publish PATTERN ...] [-subscribe PATTERN ...] [-create PATTERN ...] [-post PATTERN ...] [-redact PATTERN ...] [-sub SUBJECT] [-ttl 1h]
```

Signs a token granting, beneath `-root`, each action on the path patterns its flag names, valid for `-ttl`, at most one hour. Each action's flag may be repeated, and its patterns are relative to the root ([below](#what-a-token-grants)): `/x` is that path, `/x/` every path beneath it, `/` everything beneath the root, and a segment may be a `{name}` wildcard. With no pattern at all, the token names its key's tenant alone, as a [relay session's credential](#tickets-on-the-relay) may. `-sub` names the bearer. `-root` is a directory, `/room/123/`; a bare path (`room/123`) reads as the directory it names. For example, `qumo auth token -root /room/123/ -sub alice -post /comments -subscribe /`. The token alone goes to stdout, so `T=$(qumo auth token …)` captures it; its root, each action's patterns and when it expires go to stderr. A root outside the key's prefix, or a pattern that doesn't parse, is refused here, as the relay would refuse the token; `token.Sign` does the same. The token is presented either way, in the relay URL (`?jwt=`) or on a session's Auth Stream ([below](#tickets-on-the-relay)).

## What a token grants

A token grants what a MoQ grant does: a **`root`**, the directory of broadcast paths it applies to, and for each action the **path patterns** beneath the root its bearer may act on, on every track of each broadcast they match. It is gomoqt's `moqt.Grant`, so the relay hands the root and the `publish` and `subscribe` patterns to the session as they are.

- **`root`**: a directory of broadcast paths, written as MoQ writes one: `/acme/app/rooms/42/`, or `/` for every path. It must lie at or beneath the key's prefix, and is refused in any other spelling: without its slashes, with a doubled one, or holding a `.` or `..` segment, `*`, `{` or `}`. Every pattern lies beneath it, so it confines what the token grants to the key's tenant. Its own path is not a broadcast: no pattern reaches it, so give each broadcast a path beneath a root, as a file lies in a directory. A key confined to `/acme/app/` can't grant the broadcast `/acme/app` itself.
- **`publish`**, **`subscribe`**, **`create`**, **`post`**, **`redact`**: each a list of patterns relative to the root, matching broadcast paths as `moqt.Grant` reads them, the way an `http.ServeMux` pattern matches URL paths. With the root `/acme/app/rooms/42/`, `/7` matches `/acme/app/rooms/42/7` alone, `/7/` every path beneath it, and `/` every path beneath the root (not the root itself). A segment may be a wildcard: `{name}` matches any one segment (`/{user}/audio`), `{name...}` as the last segment the rest of the path, and `{$}` as the last segment only the `/` before it. A pattern starts with `/` and may not hold a `.` or `..` segment; one that doesn't parse, or comes without a root, refuses the token.

The actions are `publish` (sending a broadcast's tracks through a relay), `subscribe` (receiving a track, live or as history: a relay's FETCH and a funnel's `GET` read history with it), `post` (sending one message into a track that exists at a funnel, which gathers many senders into it and publishes it; a relay never reads it as publishing), `redact` (taking a record out of a funnel's track; for credentials the app keeps to itself) and `create` (creating a track at a [funnel](../funnel/#credentials), which the funnel then publishes; for the app, which decides which tracks a broadcast has).

A token may grant no pattern at all, and may then leave out `root`: it grants nothing. Whatever no pattern grants is denied, and a claim the verifier doesn't know refuses the token, so an action spelled wrong, or the `scopes` claim of earlier releases, never passes as granting nothing more. A token may name its bearer in **`sub`**, which a funnel records as the sender; without `sub` it names no one.

```json
{"root": "/acme/app/rooms/42/", "sub": "alice",
 "publish": ["/alice"], "subscribe": ["/"], "post": ["/comments"],
 "iat": 1791370000, "nbf": 1791370000, "exp": 1791370600, "jti": "…"}
```

**`path_auth` tokens are accepted too**, as other MoQ implementations sign them (the `@moq/token` convention), and read as the grant they amount to: their `root`, written as a path (`acme/app/rooms/42`), as the directory it names, `pub` as `publish`, and `sub` as `subscribe`, each on its path and every path beneath it, on every track; an empty `pub` or `sub` grants what lies beneath the root, not the root's own path. So

```json
{"path_auth": {"root": "acme/app/rooms/42", "pub": "alice", "sub": ""}, "iat": …, "nbf": …, "exp": …}
```

grants what `{"root": "/acme/app/rooms/42/", "publish": ["/alice", "/alice/"], "subscribe": ["/"]}` does. A token carrying both `path_auth` and `root` or a pattern is refused, and so is one carrying `path_auth` and `sub`. `qumo auth token` and `token.Sign` never write `path_auth`.

**`subscribe` is both live and history**, so a credential that reads a funnel track's history may also subscribe to the broadcasts it names on a relay, and one that listens live may read their history: grant patterns as narrow as the bearer needs.

**On the relay**, a session may announce a broadcast a `publish` pattern matches, and the relay takes its tracks from it. A broadcast has one publisher, so give each publisher its own broadcast path; to write into a track a [funnel](../funnel/) publishes, grant `post` there instead. The relay ignores `post` and `create`. A session may subscribe to, request the track info of and fetch from any track of a broadcast a `subscribe` pattern matches.

**A session may list announcements within its token's root**: a credential whose root is `/acme/app/rooms/42/` lists `/acme/app/rooms/42/` and what lies beneath it, and nothing of another room. Since the root lies at or beneath the key's prefix, a session lists nothing of another tenant. A credential with no `publish` or `subscribe` pattern grants nothing on the relay, not even listing.

### Tickets on the relay

A session holds more than the credential it connected with: a client presents further tokens, *tickets*, on MoQ Auth Streams, and the relay adds what each grants to the session's while it is in force. An app can then keep one long-lived session per client, connected with **no token** or with a token of **no patterns**, which names the app's key alone, and grant each room as the client joins it, with a ticket per room.

**Every grant lasts until its token expires,** wherever the token was presented: at its `exp`, with the 60 s leeway. A ticket and the credential a session connected with are the same kind of token, signed with `token.Sign`; what one is for is where it is presented. The `AUTH_OK` granting a ticket says when it lapses (its max age), so the client presents a fresh one in time, and at expiry the grant is revoked with `AUTH_ERROR` ("grant expired"). The session outlives its credential's grant: once that lapses the session may do nothing more until a ticket grants it, and it ends only when its key leaves the set. A client that stays connected longer than its credential, such as a publisher that runs for hours, presents a fresh token on an Auth Stream before the one in force expires. qumo's [funnel](../funnel/) and [HLS egress](../hls/) do this themselves when they sign their own credentials.

A ticket:

- must be signed by **the same key** as the session's credential, so everything a session may do is one tenant's;
- is verified as a credential is at connect, and a key paused with `"pause": ["publish"]` refuses one that may publish, unless the session has been granted what may publish already: a pause stops what is new, and a publisher on air renews its grant;
- is revoked too when its key leaves the set, or its key's prefix no longer holds its root, at the next 30 s check;
- that could not be checked just now, as while the relay has no usable key set, is answered `AUTH_ERROR` with `INTERNAL` (`0x1`), which the client may retry; a refused one with `UNAUTHORIZED` (`0x2`);
- is withdrawn by the client closing its Auth Stream;
- is a bearer token, as a credential is: any session of the same key's tenant that holds it may present it.

A session holds at most 33 credentials at once, its connection's own among them. An Auth Stream carrying no token is answered with the session's own grant again, for what is left of it, which adds nothing; once that grant has lapsed it is refused with `UNAUTHORIZED`. A session granted a ticket that may publish is reported as a publisher from then on, for the rest of its life ([usage](../../configuration/#usage-reports)). A relay with auth off, or a relay peer, takes no tickets: it answers `NOT_SUPPORTED`, and its own grant never lapses.

**A session may connect with no token** (no `jwt` in its URL; an empty `jwt=` is a token that doesn't verify, and is refused). It is admitted granted nothing, and an Auth Stream carrying no token is answered with that empty grant. Its first ticket may be signed by any key in the set, and binds the session to that key: every later ticket must be signed by it, as one of a session that connected with a token must be signed by that token's key, and the session's usage is reported under it from then on. A session granted no ticket within 10 s of connecting is closed with `UNAUTHORIZED` ("no credential presented in time"), so no one holds a session open granted nothing. Once one is granted, the session stays open as any other does, even after its tickets lapse.

## Signing in your app (Go)

```go
key, err := token.LoadSigningKey("signing-key.jwk") // once, at start
// per client, after your app has decided what this user may do:
tok, err := token.Sign(key, token.Grant{
	Root:    "/acme/app/rooms/42/", // at or beneath the key's prefix
	Subject: "alice",
	// publish every track of the broadcast acme/app/rooms/42/alice
	Publish: []string{"/alice"},
	// watch and read the history of every broadcast in the room
	Subscribe: []string{"/"},
	// post into the tracks of its comments broadcast at a funnel, as alice
	Post: []string{"/comments"},
}, time.Hour)
```

`token.Grant` embeds the `moqt.Grant` a relay session receives, as it is, and also matches a path as the relay and funnel do (`Allows`). `token.Directory` spells a path as a root (`acme/app` is `/acme/app/`).
