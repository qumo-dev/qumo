---
title: TLS & mTLS
description: Certificates for the relay, and mutual TLS between peer relays.
weight: 4
---

## Server TLS

qumo requires TLS 1.3 for its QUIC listener, configured via `CERT_FILE` /
`KEY_FILE` (see [Configuration → Server]({{< relref "../configuration" >}}#server)
for the full variable reference).

For local development, generate a browser-trusted cert with
[mkcert](https://github.com/FiloSottile/mkcert):

```bash
mkcert -install
mkcert -cert-file certs/server.crt -key-file certs/server.key localhost 127.0.0.1 ::1
qumo relay
```

(`qumo playground` needs no manual cert — it generates and trusts its own dev
certificate automatically.)

## Trusted peers (optional)

Setting `CA_FILE` makes the relay's peers identify themselves by certificate:

- a session presenting a client certificate signed by this CA is a trusted
  peer, never asked by the auth server;
- a client certificate stays optional, so browsers (which present none) still
  connect and are admitted by the auth server;
- the dialer presents this node's `CERT_FILE` cert to the relays in `PEERS` and
  verifies theirs against the system roots plus this CA.

Without `CA_FILE`, no session is a peer. See
[Configuration → Peer trust]({{< relref "../configuration" >}}#peer-trust-optional).
