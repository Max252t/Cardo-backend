# Changelog

## 1.0.0 - 2026-10-30

First release of the Cardo backend.

### Foundation
- Environment-based configuration with validation.
- PostgreSQL migrations on startup under an advisory lock.
- Registration and login with argon2id, JWT access tokens and rotating refresh tokens with reuse detection.
- Atomic per-IP login rate limit, profile endpoints, board REST API.
- `/healthz` and `/readyz`, graceful shutdown with a readiness drain.

### Board operations
- Fractional `rank` ordering for columns and cards.
- `OpProcessor` serialising operations per board under a row lock, with `processed_ops` idempotency, version conflicts and nacks.
- Board snapshot and event log endpoints with `410 Gone` outside retention.

### Realtime
- WebSocket hub with handshake (`hello` → `sync.delta|sync.snapshot` → `presence.list` → `sync.ready`), heartbeat, per-connection op rate limit and close codes.
- Redis pub/sub fan-out across instances and Redis presence with atomic Lua join and leave.

### Members and invites
- Member roster with `owner`, `editor` and `viewer` roles and `member.*` events.
- Hashed, expiring, use-limited invite tokens.
- Live role updates and `4003` closes with membership revalidation.

### Deployment and observability
- Docker Compose stack with two API replicas behind nginx, PostgreSQL and Redis, plus Prometheus and Grafana under the `monitoring` profile.
- Prometheus metrics on a dedicated `METRICS_ADDR` listener: `ws_connections`, `ops_total`, `op_apply_duration_seconds`, `event_publish_errors_total`, `ws_closed_total`, `http_request_duration_seconds`, `resync_total`, retention and rate limit metrics.
- `healthcheck` subcommand for container health checks.

### Retention and rate limiting
- Hourly retention of `board_events` (7 days, at least the newest 1000 per board) and `processed_ops` (24 hours), run by one replica at a time, deleting per-board prefixes in bounded batches.
- `resync_required` nack when a replayed operation's event has been pruned.
- Per-user REST rate limit (Redis GCRA or in-memory) with `429` and `Retry-After`, fail-open with a circuit breaker.
- Neighbour-only rank reads for card and column placement.

### Verification
- Convergence and event-loss tests with simulated clients on two instances over real PostgreSQL and Redis.
- `cmd/loadtest` tool and `docs/loadtest-report.md`: p95 send-to-receive latency 10 ms with 50 clients at 20 ops/s, resync after 1000 missed events in 342 ms worst case.

### Documentation
- OpenAPI 3.1 specification in `docs/openapi.yaml`, kept in sync with the router by a test.
- README covering layout, deployment, configuration, protocol, metrics, retention and CI.
