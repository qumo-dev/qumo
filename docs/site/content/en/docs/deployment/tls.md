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

Relays authenticate each other with mutual TLS: a relay that dials another
(`PEERS`) presents its `CERT_FILE` as its client certificate.

**Relays that share a certificate are peers,** with nothing more to set. A
session that presents the dialed relay's own certificate is a trusted peer,
whose credential is never checked. Only a holder of the certificate's private
key can do that. For a fleet behind one wildcard certificate, list the other
relays in `PEERS` by names the certificate covers, and they peer. While a
certificate is being replaced, relays on the old and the new one are not
peers of each other until both run the new one.

Setting `CA_FILE` trusts a private CA instead, for relays with a certificate
each:

- a session presenting a client certificate signed by this CA is a trusted
  peer, whose credential is never checked;
- a client certificate stays optional, so browsers (which present none) still
  connect and are admitted by their credential;
- the dialer presents this node's `CERT_FILE` cert to the relays in `PEERS` and
  verifies theirs against the system roots plus this CA.

See
[Configuration → Peer trust]({{< relref "../configuration" >}}#peer-trust-optional).
