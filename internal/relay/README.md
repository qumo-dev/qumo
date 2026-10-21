# Relay Package

MoQT (Media over QUIC Transport) relay implementation for qumo, including the
server wrapper, the track-wide broadcast distributor, the group cache, and the
frame pool.

This file covers the package internals. For production configuration (env vars,
TLS, peer/mTLS), see [`relay-config.example.env`](../relay-config.example.env)
and [docs/site/content/en/docs/deployment/tls.md](../docs/site/content/en/docs/deployment/tls.md).
For the top-level feature list, see [`README.md`](../README.md).

## Core Components

- **server.go** — MoQT server wrapper: initialization, lifecycle, and shutdown.
- **handler.go** — Relay handler with the track distributor (broadcast channel pattern).
- **group_cache.go** — Ring buffer for group caching with atomic reads.
- **frame_pool.go** — `sync.Pool`-based frame allocation.
- **config.go** — Configuration structures.

## Design Patterns

### Broadcast Channel Pattern

Efficient fan-out to many subscribers:

- Each subscriber gets its own buffered notification channel.
- A small notification timeout balances CPU and latency.
- Zero allocations during steady-state operation.
- Thread-safe subscribe/unsubscribe via `RWMutex`.

### Frame Pooling

Reusable frame allocation via `sync.Pool`:

- Configurable frame capacity (MTU-sized by default).
- Automatic frame reset on return to the pool.
- Thread-safe concurrent access.

### Ring Buffer Cache

Fixed-size group cache with atomic reads:

- Configurable size.
- Constant-time access.
- Lock-free reads via atomic pointers.
- Automatic eviction of old groups.

## Running Tests

```bash
# Standard
go test ./... -v

# With coverage
go test ./... -cover

# With the race detector
go test ./... -race
```

Benchmarks are in `*_bench_test.go` files; run them with `go test -bench=. -benchmem`.

## Integration Notes

- Configuration is driven by environment variables; see
  [`relay-config.example.env`](../relay-config.example.env).
- TLS is mandatory for the relay. Generate a dev cert with `mage cert`
  (repo root); see the deployment TLS doc linked above for production peer/mTLS.
- Production runtime tuning (e.g. GC target for high-fan-out) is documented in
  [docs/perf/README.md](../docs/perf/README.md) and surfaced by `qumo doctor`.

## Future Improvements

- Integration tests with real `moqt.TrackReader`/`moqt.TrackWriter`.
- Metrics/observability hooks.
- Configurable broadcast channel buffer size.
- Dead subscriber detection and cleanup.
