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

## Mutual TLS between peers (optional)

Setting `CA_FILE` enables mutual TLS for the relay's peer mesh:

- incoming connections must present a client cert signed by this CA;
- the dialer presents this node's `CERT_FILE` cert to remote relays and trusts only the CA pool.

Because mTLS is required by default once `CA_FILE` is set, browsers — which
don't present a client cert — can no longer connect. If the same relay also
serves browser/WebTransport traffic directly, set `MTLS_REQUIRED=false`: peer
certs are still verified when presented, but connections without one are
accepted. See
[Configuration → mTLS]({{< relref "../configuration" >}}#mtls-optional) for
the `CA_FILE` / `MTLS_REQUIRED` variable reference.
