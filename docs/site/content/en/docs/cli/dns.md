---
title: dns
description: Answer one DNS name with the addresses of the region nearest the client.
weight: 6
---

An authoritative DNS server for one zone. It answers one name in that zone
with the addresses of the region nearest the client, so that one hostname
sends every client to a nearby relay. Run it beside each relay: every place
that serves clients then also answers where to connect, and when a place is
down, resolvers ask the others.

It keeps no inventory. A **feed**, read over HTTP(S) on an interval, lists the
regions, where each is, and its addresses. The server answers from the last
good reading, keeps that reading on disk, and goes on answering from it while
the feed cannot be read, including after a restart. A feed it cannot use
never replaces one it can.

A client is placed with a city database in MaxMind's `.mmdb` format, by the
subnet its resolver sends ([EDNS Client Subnet](https://www.rfc-editor.org/rfc/rfc7871))
or else by the resolver's own address. The answer holds up to
`QUMO_DNS_MAX_ANSWERS` addresses, all of the nearest region that has an
address of the type asked, rotated across queries. It never mixes regions:
resolvers and clients reorder the addresses of an answer, so a far region's
address would be the first some clients try. A client the database cannot
place, or every client when there is no database, gets the regions in turn.

Taking a region that is down out of the feed is the feed's job: until it
does, that region's clients are answered with it.

It does not recurse: a name outside its zone is refused.

```sh
QUMO_DNS_ZONE=geo.example.com \
QUMO_DNS_NAME=cdn.geo.example.com \
QUMO_DNS_NAMESERVERS=ns1.example.com,ns2.example.com \
QUMO_DNS_FEED_URL=https://control.example.com/relays/dns \
QUMO_DNS_STATE_DIR=/var/lib/qumo-dns \
QUMO_DNS_GEOIP_URL='https://download.db-ip.com/free/dbip-city-lite-{month}.mmdb.gz' \
qumo dns
```

## Delegation

Delegate the zone to the hosts that run `qumo dns`, with NS records in the
parent zone, and give those hosts addresses there:

```
geo.example.com.      NS  ns1.example.com.
geo.example.com.      NS  ns2.example.com.
ns1.example.com.      A   192.0.2.10
ns2.example.com.      A   198.51.100.20
cdn.example.com.      CNAME cdn.geo.example.com.
```

A CNAME from a name in the parent zone, as on the last line, lets clients
use a name outside the delegated zone, and keeps names beside it (such as an
ACME `_acme-challenge` record for that name) in the parent zone.

## The feed

`GET` on `QUMO_DNS_FEED_URL` answers `200` with:

```json
{
  "regions": [
    {"region": "a", "lat": 35.68, "lon": 139.69, "addrs": ["192.0.2.10", "2001:db8::10"]},
    {"region": "b", "lat": 43.06, "lon": 141.35, "addrs": ["198.51.100.20"]}
  ],
  "published_at": "2026-01-01T00:00:00Z"
}
```

- `lat`/`lon` place the region, in degrees.
- `addrs` are public unicast IPv4 (answered as A) and IPv6 (AAAA) addresses.
  Private, loopback and malformed entries are left out and logged.
- `published_at` becomes the SOA serial (Unix seconds). A feed should move
  it only when the regions or addresses change: a body read again unchanged
  is not written to disk again.
- A feed with no region left that has an address is refused; what is
  served stays.
- `503` means the feed has nothing yet; what is served stays. Any other
  failure is logged and retried on the next interval.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `QUMO_DNS_ZONE` | required | The zone this server is authoritative for. |
| `QUMO_DNS_NAME` | the zone | The name answered with addresses; must be in the zone. Other names in the zone are NXDOMAIN. |
| `QUMO_DNS_NAMESERVERS` | required | The zone's NS set, comma-separated; the first is the SOA's primary. |
| `QUMO_DNS_FEED_URL` | required | Where the regions are read. |
| `QUMO_DNS_FEED_TOKEN` | none | Sent as `Authorization: Bearer`. |
| `QUMO_DNS_FEED_INTERVAL` | `15s` | How often the feed is read. |
| `QUMO_DNS_STATE_DIR` | none | Keeps the last good feed (`feed.json`) and a downloaded database (`city.mmdb`). |
| `QUMO_DNS_GEOIP_DB` | `<state dir>/city.mmdb` with a URL | A city database to place clients with. |
| `QUMO_DNS_GEOIP_URL` | none | Where that database is downloaded; `{month}` becomes `2006-01`, this month's file then last month's. A `.gz` path is decompressed. Checked with `If-Modified-Since`. |
| `QUMO_DNS_GEOIP_INTERVAL` | `24h` | How often the download is checked. |
| `QUMO_DNS_LISTEN` | `:53` | UDP and TCP address. On a host whose own resolver listens on `127.0.0.53:53`, bind the public address. |
| `QUMO_DNS_HTTP_ADDR` | `127.0.0.1:9153` | `/healthz` (200 once a feed is served) and `/metrics`; `off` for none. |
| `QUMO_DNS_TTL` | `30s` | TTL of the address answers. |
| `QUMO_DNS_MAX_ANSWERS` | `2` | Addresses in one answer. |
| `QUMO_DNS_HOSTMASTER` | `hostmaster.<zone>` | The SOA contact. |

A location database has its own licence. [DB-IP's City Lite](https://db-ip.com/db/download/ip-to-city-lite)
is free under CC BY 4.0, which asks you to credit DB-IP where the data is
used. MaxMind's GeoLite2 City needs an account and a licence key in the URL;
the key is never logged.

## Metrics

| Series | |
|---|---|
| `qumo_dns_queries_total{rcode}` | Queries answered, by response code. |
| `qumo_dns_client_locations_total{outcome}` | Address queries whose client was `located` or `unknown`. |
| `qumo_dns_feed_rounds_total{outcome}` | Feed reads: `updated`, `unchanged`, `not_ready`, `error`. |
| `qumo_dns_feed_last_success_timestamp_seconds` | When the feed was last read. |
| `qumo_dns_geo_refresh_total{outcome}` | Database refreshes: `updated`, `unchanged`, `error`. |
| `qumo_dns_zone_regions`, `qumo_dns_zone_addresses` | What is served. |
