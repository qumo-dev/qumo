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
  verifies is a relay peer when the certificate carries the peering name
  `peer.qumo.internal`. A peer is served without a credential. A certificate
  without the name grants nothing: its session needs a credential like any
  client's. A browser is never asked for a certificate.
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

A certificate from the same CA **without** the peering name grants nothing.
Whatever holds a peer certificate is trusted like a relay, so issue one only
to what runs under the same control as the relays. The HLS egress can connect
with one (`RELAY_CERT_FILE`), or with a signing key instead
(`RELAY_SIGNING_KEY`). Each holder needs a certificate of its own: a relay
drops a session that presents the relay's own certificate, as itself.

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
- **Renewal needs no restart.** The relay re-reads its peer certificate and
  `CA_FILE` about once a minute ([Certificate renewal](#certificate-renewal)).
  Renew them before the old certificate expires: a relay still holding an
  expired one can neither dial nor be dialed.
- **A chain is fine.** When an intermediate CA issues the certificate, put
  the intermediate after the leaf in `PEER_CERT_FILE` and the root in
  `CA_FILE`.
- **Revocation lists are not checked.** A compromised certificate stays valid
  until it expires or the CA is replaced.

See
[Configuration → Peer trust]({{< relref "../configuration" >}}#peer-trust-optional).

## Certificate renewal

The relay re-reads `CERT_FILE` / `KEY_FILE`, `PEER_CERT_FILE` /
`PEER_KEY_FILE` and `CA_FILE` about once a minute, and compares what they hold
with what it is using. Whatever renews them — cert-manager, a Vault or step-ca
agent writing files, a cron job — needs only to write the new files; the relay
needs no restart and no signal.

- **New handshakes only.** A renewed certificate is presented from the next
  TLS handshake on, to browsers, native-QUIC clients and peers alike, and a
  renewed `CA_FILE` verifies the next peer that dials or is dialed. Sessions
  already established are untouched: they keep the connection, and the
  certificate, they were set up with.
- **Taken whole, or not at all.** A public certificate is taken only once it
  parses and matches its key. A peer certificate and `CA_FILE` are taken
  together, only when they check out as at startup: the certificate is one the
  CA issued, carries the peering name, and is valid now. Anything else (a
  half-written file, a key that doesn't match, a certificate from a CA not in
  `CA_FILE`, one that has expired) is logged as an error and the certificates
  in use are kept; the next read tries again. The error is logged once, and
  again only if it changes, not at every read; a later read that takes a
  renewal, or finds the files back to what is in use, clears it.
- **Replace the files atomically.** Write each new file beside the old and
  rename it into place. A read between two writes in place sees a new
  certificate beside the old key, refuses the pair and logs an error, and the
  next read after both are written takes it.
- **Rotating the relay CA.** Put the new CA beside the old one in every
  relay's `CA_FILE` first, wait for the relays to read it, then issue peer
  certificates from the new CA, and drop the old CA from `CA_FILE` once no
  relay presents a certificate from it. A peer certificate from a CA the relay
  doesn't yet trust is refused, and the old one kept.
- **Requests to the control plane** — the relay's key-set and usage
  requests — are signed with the renewed peer certificate from the next
  request on. [`qumo dns`]({{< relref "../cli/dns" >}}) re-reads its
  `PEER_CERT_FILE` / `PEER_KEY_FILE` the same way, about once a minute, and
  signs its feed reads with a renewed pair from then on.

The relay logs each certificate it takes, with its serial and expiry, and
counts reads in `qumo_relay_cert_reloads_total{cert,result}` (`qumo dns`:
`qumo_dns_cert_reloads_total{cert,result}`).
`qumo_relay_cert_not_after_seconds{cert}` (`qumo dns`:
`qumo_dns_cert_not_after_seconds{cert}`) is when the certificate in use
expires. **Alert on that gauge** — for example, when it is less than a few days
away — rather than on single reload errors: a renewal written in place can
log one harmless error, and a certificate that is never renewed shows only as
an expiry that keeps getting closer
([Observability]({{< relref "../observability" >}})).

## Clients verify the relay by default

qumo's servers (`relay`, `rtmp`, `rtsp`, `rtsp-push`, `playground`) present TLS from `CERT_FILE` / `KEY_FILE` (default `certs/server.crt` + `.key`; generate one with `mage cert`). The clients that dial a relay **verify its certificate by default** — verification is the default, not an opt-in:

- `qumo loadgen` and `tools/capacity` require the relay's cert as a trust anchor (`--ca <cert.pem>`), or `--insecure` for a self-signed dev relay. Verification is the default; `--insecure` is the explicit escape hatch.
- The bench/test clients follow the same convention: `bench-multiproc/tools/latency-probe` takes `--ca` or `--insecure`; `internal/smoketest` (the `mage smoke` harness) takes `-ca` or `-insecure`, with `mage smoke` passing `-insecure` against the self-signed Docker topology.
- `qumo hls` verifies against the system root store by default. Trust a specific relay cert with `RELAY_CA_FILE`, or disable verification for a self-signed dev relay with `RELAY_TLS_INSECURE=true`:

  ```bash
  qumo hls                                   # verify against the system roots
  RELAY_CA_FILE=certs/server.crt qumo hls    # trust this relay's cert
  RELAY_TLS_INSECURE=true qumo hls           # dev relay with a self-signed cert
  ```

`cmd/seed-moq`, the dev seeder, presents an ephemeral self-signed certificate — point the egress at it with `RELAY_TLS_INSECURE=true`.
