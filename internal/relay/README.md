# Relay Package

MoQT (Media over QUIC Transport) relay implementation for qumo, including the
server wrapper, the track-wide broadcast distributor, the group cache, and the
frame pool.

This file covers the package internals. For production configuration (env vars,
TLS, peer/mTLS), see [`relay-config.example.env`](../../relay-config.example.env)
and [docs/site/content/en/docs/deployment/tls.md](../../docs/site/content/en/docs/deployment/tls.md).
For the top-level feature list, see [`README.md`](../../README.md).

## Core Components

- **server.go** — MoQT server wrapper: initialization, lifecycle, and shutdown.
- **handler.go** — Relay handler with the track distributor (broadcast channel pattern).
- **group_cache.go** — Ring buffer for group caching with atomic reads.
- **frame_pool.go** — `sync.Pool`-based frame allocation.
- **config.go** — Configuration structures.

## Design Patterns

### Broadcast Notification

Constant-time fan-out to many subscribers (`broadcast_notify.go`):

- One shared notifier per track: an atomic sequence number and a channel that
  is closed and swapped for a fresh one on every notification, waking every
  waiting subscriber at once.
- A subscriber reads the sequence and the channel together, so it never misses
  a notification between checking and waiting.
- No per-subscriber channel, no subscriber list lock, and no poll timer: each
  frame's notification is the only wakeup.

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

Benchmarks are in `benchmarks_test.go` and the `*_bench_test.go` files; run them with `go test -bench=. -benchmem`.

## Integration Notes

- Configuration is driven by environment variables; see
  [`relay-config.example.env`](../../relay-config.example.env).
- TLS is mandatory for the relay. Generate a dev cert with `mage cert`
  (repo root); see the deployment TLS doc linked above for production peer/mTLS.
- Production runtime tuning (e.g. GC target for high-fan-out) is documented in
  [docs/perf/README.md](../../docs/perf/README.md) and surfaced by `qumo doctor`.

## Future Improvements

- Integration tests with real `moqt.TrackReader`/`moqt.TrackWriter`.
- Metrics/observability hooks.
- Dead subscriber detection and cleanup.
