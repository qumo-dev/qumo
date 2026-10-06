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

## Relay peers (optional)

Relays authenticate each other with **mutual TLS under a relay CA**, a CA you
run. Public certificates such as Let's Encrypt's are for browsers and other
clients; they play no part in relay identity.

- **Inbound:** a native-QUIC session that presents a certificate `CA_FILE`
  verifies is an internal client, or a relay peer when the certificate carries
  the peering name `peer.qumo.internal`. A peer is served without a credential.
  An internal client (the HLS egress with `RELAY_CERT_FILE`) may subscribe to
  anything and announce nothing. A browser is never asked for a certificate.
- **Outbound:** a relay dials each address in `PEERS` presenting its own peer
  certificate (`PEER_CERT_FILE`, `PEER_KEY_FILE`), asks for the server name
  `peer.qumo.internal`, and verifies the certificate the peer answers with
  against `CA_FILE` only. The dialed relay answers that name with its peer
  certificate, not its public one. So `PEERS` may name peers by any address:
  a Consul name, an IP.
- **Itself:** a relay whose `PEERS` resolve to it (a group name) recognizes its
  own certificate on that session, drops it, and doesn't retry.

### What a peer certificate must contain

Issue one per relay from the relay CA:

- a DNS subject alternative name `peer.qumo.internal` (the peering name);
- the subject common name set to the relay's identity, such as its node
  name; the relay logs it and never interprets it;
- extended key usages for both client authentication and server
  authentication, since a relay both dials and is dialed;
- a validity the relay is within; it refuses to start with an expired one.

A certificate from the same CA **without** the peering name authenticates an
internal client, such as the HLS egress: issue those without it.

With OpenSSL:

```bash
# The relay CA, once.
openssl ecparam -genkey -name prime256v1 -noout -out relay-ca.key
openssl req -x509 -new -key relay-ca.key -sha256 -days 3650 \
  -subj "/CN=relay CA" -out relay-ca.crt

# One peer certificate per relay (here relay-1).
openssl ecparam -genkey -name prime256v1 -noout -out relay-1.key
openssl req -new -key relay-1.key -subj "/CN=relay-1" -out relay-1.csr
printf 'subjectAltName=DNS:peer.qumo.internal\nextendedKeyUsage=clientAuth,serverAuth\n' > relay-1.ext
openssl x509 -req -in relay-1.csr -CA relay-ca.crt -CAkey relay-ca.key -CAcreateserial \
  -days 365 -sha256 -extfile relay-1.ext -out relay-1.crt
```

Then on each relay: `CA_FILE=relay-ca.crt`, `PEER_CERT_FILE=relay-1.crt`,
`PEER_KEY_FILE=relay-1.key`, and `PEERS` for the relays it dials. Any CA
tooling works the same way (step-ca, Vault PKI). For development, `mage cert`
writes a relay CA and peer certificates (`PEER_NAMES=a,b mage cert`).

Issuing, renewing, distributing and revoking these certificates is the
operator's; the relay only reads the files.

- **One certificate per relay.** A relay recognizes itself by its own
  certificate. Two relays given the same one each take the other for
  themselves, drop the session without an error, and never peer.
- **Renewal needs a restart.** The relay reads its peer certificate once, at
  startup. Renew it on disk and restart the relay before the old one expires;
  a relay still running on an expired certificate can neither dial nor be
  dialed.
- **A chain is fine.** When an intermediate CA issues the certificate, put
  the intermediate after the leaf in `PEER_CERT_FILE` and the root in
  `CA_FILE`.
- **Revocation lists are not checked.** A compromised certificate stays valid
  until it expires or the CA is replaced.

See
[Configuration → Peer trust]({{< relref "../configuration" >}}#peer-trust-optional).
