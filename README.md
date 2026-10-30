# Cardo Backend

Backend for **Cardo** — a collaborative real-time Kanban board (Trello-lite) with a Flutter mobile client.
Several users move and edit cards on the same board simultaneously and see each other's changes instantly.

## Contents

- [Highlights](#highlights)
- [Tech stack](#tech-stack)
- [Architecture](#architecture)
- [Project layout](#project-layout)
- [Getting started](#getting-started)
- [Configuration](#configuration)
- [API overview](#api-overview)
- [WebSocket protocol](#websocket-protocol)
- [Metrics](#metrics)
- [Retention](#retention)
- [REST rate limit](#rest-rate-limit)
- [Load test](#load-test)
- [Tests](#tests)
- [Development workflow and CI](#development-workflow-and-ci)

## Highlights

- Real-time sync over WebSocket with strict event ordering (`seq`) and gap recovery from an event log
- Server-side serialization of board operations via `SELECT … FOR UPDATE` on the board row
- Optimistic concurrency: card/column moves checked by `version`, description edits by `description_version`, titles are last-writer-wins
- Fractional indexing (`rank`, base62) for ordering — no mass renumbering
- Idempotent operations: client-generated UUIDs and a `processed_ops` table
- RBAC (`owner` / `editor` / `viewer`) applied to live connections immediately
- Horizontal scaling without sticky sessions: Redis Pub/Sub for events, Redis presence with TTL
- JWT access tokens + rotating refresh tokens with reuse detection
- Prometheus metrics on a separate listener, retention jobs, per-user REST rate limit with a circuit breaker
- OpenAPI 3.1 specification kept in sync with the router by a test

## Tech stack

| Layer | Technology |
|-------|------------|
| Language | Go 1.26+ |
| HTTP | Gin |
| WebSocket | gorilla/websocket |
| Database | PostgreSQL 15+ (pgx), `postgres:16-alpine` in Compose |
| Cache / Pub/Sub / presence | Redis 7+ (go-redis) |
| Auth | golang-jwt, argon2id |
| Migrations | golang-migrate |
| Observability | slog (JSON), Prometheus |
| Tests | testing, testify, testcontainers-go |

## Architecture

```
┌──────────────┐  HTTPS / WSS   ┌────────┐      ┌────────────────────────────┐
│   Flutter    │ ◄────────────► │ nginx  │ ◄──► │ Go API #1 … #N (Gin)       │
│   client     │                └────────┘      │  ├─ REST  /api/...         │
└──────────────┘                                │  ├─ WS    /ws/boards/:id   │
                                                │  ├─ Hub (local rooms)      │
                                                │  └─ OpProcessor            │
                                                └──────┬──────────────┬──────┘
                                                       │              │
                                          ┌────────────▼───┐   ┌──────▼──────────┐
                                          │  PostgreSQL    │   │  Redis          │
                                          │  source of     │   │  Pub/Sub events │
                                          │  truth +       │   │  presence (TTL) │
                                          │  board_events  │   │  rate limiting  │
                                          └────────────────┘   └─────────────────┘
```

Principles:

1. **PostgreSQL is the single source of truth.** Any state can be rebuilt from the database and the event log.
2. **Serialization in the database.** Operations on one board run strictly sequentially thanks to the row lock on `boards`, regardless of the API instance.
3. **Redis Pub/Sub is best-effort.** Clients detect lost events by `seq` gaps and fetch them from the log.
4. **Single write path.** Every board content change goes through `OpProcessor`.

### Operation to event flow

```mermaid
flowchart LR
    C1[Client A] -- "op.card.move (op_id)" --> H1[Hub on instance 1]
    H1 --> P[OpProcessor]
    P -- "BEGIN, lock board row" --> DB[(PostgreSQL)]
    P -- "processed_ops check, apply, board_events INSERT" --> DB
    DB -- COMMIT --> P
    P -- "publish after commit" --> R[(Redis Pub/Sub)]
    R --> H1
    R --> H2[Hub on instance 2]
    H1 -- "event (seq N)" --> C1
    H1 -- "event (seq N)" --> C3[Client C]
    H2 -- "event (seq N)" --> C2[Client B]
    C2 -. "seq gap: GET /api/boards/id/events?after_seq=" .-> REST[REST on any instance]
    REST --> DB
```

The sender gets the event with its own `op_id` back, which is the acknowledgement. A failed publish is counted in `event_publish_errors_total` and never fails the operation: clients recover missing events by `seq`.

## Project layout

```
cmd/
  api/            the only composition root: wiring, servers, graceful shutdown, healthcheck subcommand
  loadtest/       WebSocket load-testing tool
internal/
  domain/         entities, errors, validation (stdlib + uuid only)
  rank/           fractional indexing
  clock/          clock and timer ports
  secrets/        token generation and hashing
  boardcontent/   operations, events, nacks: the board content shapes
  boardtx/        transaction ports for board content and administration
  boardevents/    event publisher port
  ops/            OpProcessor: the operation pipeline
  boards/         board use cases, snapshot, event log, WebSocket catch-up policy
  users/          profile use cases
  auth/           registration, login, refresh rotation, login rate limit port
  presence/       presence use case
  members/        roster and invites
  retention/      retention job service
  protocol/       wire format: envelope v1, session messages, close codes, channel names
  storage/        PostgreSQL repositories, migrations runner, retention queries
  pubsub/         Redis and in-memory event fan-out
  presencestore/  Redis presence with atomic Lua scripts
  ratelimit/      login limiter, per-user REST limiter (Redis GCRA, in-memory), circuit breaker
  jwtauth/        JWT access tokens
  argon2id/       password hashing with a concurrency limit
  metrics/        Prometheus collector
  httpapi/        Gin router, handlers, middleware
  realtime/       WebSocket hub, clients, handshake
  config/         env-based configuration
  realtime/rttest, testutil           test support
  simclient/, convergence/            simulated clients and convergence tests
migrations/       SQL migrations
deploy/           nginx, Prometheus and Grafana configs, deployment tests
docs/             OpenAPI specification, load test report
```

Layers, from the innermost outwards. Dependencies only point inward.

```
domain, rank, clock, secrets                         innermost, stdlib + uuid only
boardcontent, boardtx, boardevents                   inner shapes and ports
ops, boards, users, auth, presence, members,
retention                                            use cases
protocol                                             outer wire format: envelope v1, session messages, close codes, channels
storage, pubsub, presencestore, ratelimit,
jwtauth, argon2id, metrics                           adapters
httpapi (Gin), realtime (gorilla)                    delivery
cmd/api, cmd/loadtest                                executables
realtime/rttest, testutil, simclient, convergence    test support, never imported by production code
```

The layering is enforced by `depguard` in `.golangci.yml`:

- `inner-layer`: the innermost packages may import only the standard library, `uuid` and each other.
- `use-cases`: use cases may import only the standard library, `uuid` and the inner packages. They declare the ports they need and never import an adapter.
- `production-code`: production code must not import `realtime/rttest`, `testutil` or `simclient`.
- `metrics-adapter`: `internal/metrics` and the Prometheus client may be used only from `cmd/api`.

## Getting started

### Prerequisites

- Go 1.26+
- Docker and Docker Compose v2

### Run with Docker Compose

```bash
cp .env.example .env
```

Set `JWT_SECRET` in `.env` to a random string of at least 32 bytes (compose refuses to start without it), then:

```bash
docker compose up --build
```

Migrations run automatically on startup under an advisory lock, so both API replicas can start at the same time.

Topology (all services share the `cardo` network, `172.28.0.0/16`):

| Service | Role | Address |
|---------|------|---------|
| `nginx` | Single entry point, round-robin without sticky sessions | `172.28.0.10`, published on host port `8080` |
| `api-1` | API replica | `172.28.0.11:8080`, metrics on `:9091` |
| `api-2` | API replica | `172.28.0.12:8080`, metrics on `:9091` |
| `postgres` | `postgres:16-alpine` with the `postgres-data` volume | internal |
| `redis` | `redis:7-alpine` | internal |
| `prometheus`, `grafana` | `monitoring` profile only | `127.0.0.1:9090`, `127.0.0.1:3000` |

The API is reachable at `http://localhost:8080`. All services have health checks and the API replicas wait for healthy Postgres and Redis; nginx waits for both replicas. The API image is distroless and its health check is the `/api healthcheck` subcommand, which calls `/readyz` on localhost.

Compose pins `SINGLE_INSTANCE=false`, `REDIS_URL`, `DATABASE_URL`, `HTTP_ADDR`, `TRUSTED_PROXIES` and `METRICS_ADDR` in the service definition, so changing them in `.env` has no effect for the stack. Everything else in `.env` (token TTLs, rate limits, retention, `LOG_LEVEL`, …) is passed to both replicas. `POSTGRES_PASSWORD` is used by Postgres and to build the `DATABASE_URL`.

Monitoring is opt-in:

```bash
docker compose --profile monitoring up --build
```

Prometheus (`http://127.0.0.1:9090`) scrapes `api-1:9091` and `api-2:9091` every 5 s. Grafana (`http://127.0.0.1:3000`, user `admin`, password `GRAFANA_ADMIN_PASSWORD` from `.env`) has the Prometheus data source and a provisioned Cardo dashboard. Both ports are bound to `127.0.0.1` only.

nginx behaviour (`deploy/nginx.conf`):

- Round-robin over `172.28.0.11:8080` and `172.28.0.12:8080` with `max_fails=1 fail_timeout=10s`. A request that fails with a connection error, timeout or `503` is retried once on the other replica.
- `/ws/` is proxied with the `Upgrade`/`Connection` headers and a 120 s read/send timeout, longer than the 30 s ping and 60 s pong window.
- `X-Real-IP` and `X-Forwarded-For` carry the client address. The API trusts only `172.28.0.10/32` (`TRUSTED_PROXIES`), so the per-IP login limit sees real client addresses.
- `client_max_body_size 1m` matches the default `HTTP_MAX_BODY_BYTES`.
- `/metrics` is answered with `404` by nginx. It is not on the API listener anyway: metrics are served on `METRICS_ADDR` (`:9091`) and scraped directly on the compose network.
- Paths under `/api/invites/` are not logged (no access log, error log restricted to `crit`), because the invite token is part of the URL. The access log format also never records the raw request line.

Failover and graceful shutdown:

- On `SIGTERM` an instance first flips `/readyz` to `503 shutting_down` for `SHUTDOWN_DRAIN_DELAY` (3 s) while still serving, then closes every WebSocket with code `1001`, finishes in-flight requests (up to `SHUTDOWN_TIMEOUT`, 20 s) and exits. New WebSocket upgrades during shutdown get `503 shutting_down`. The `api` containers have `stop_grace_period: 30s`.
- Clients reconnect through nginx, which sends them to the surviving replica, and resynchronise with `hello{last_seq}`.
- If a replica dies without draining (`docker compose kill api-1`), its sockets drop, nginx marks it failed for 10 s and routes new connections and retries to `api-2`. Clients detect the gap by `seq` or by the reconnect and converge to the database state.

### Run locally

Start PostgreSQL and run the API in single-process mode (in-memory pub/sub, presence and login limiter, no Redis):

```bash
export DATABASE_URL='postgres://cardo:cardo@localhost:5432/cardo?sslmode=disable'
export JWT_SECRET='a-random-string-of-at-least-32-bytes'
export SINGLE_INSTANCE=true
go run ./cmd/api
```

`SINGLE_INSTANCE=true` must never be used with more than one replica. Set `METRICS_ADDR=:9091` to expose Prometheus metrics locally.

## Configuration

All settings come from environment variables; see `.env.example` for a ready-to-use set. Durations use Go syntax (`15s`, `168h`).

| Variable | Default | Notes |
|----------|---------|-------|
| `HTTP_ADDR` | `:8080` | API listen address. |
| `HTTP_READ_HEADER_TIMEOUT` | `5s` | |
| `HTTP_READ_TIMEOUT` | `15s` | |
| `HTTP_WRITE_TIMEOUT` | `15s` | |
| `HTTP_IDLE_TIMEOUT` | `60s` | |
| `HTTP_MAX_BODY_BYTES` | `1048576` | Must be positive. Larger bodies get `413 payload_too_large`. Keep nginx `client_max_body_size` consistent. |
| `SHUTDOWN_TIMEOUT` | `20s` | Must be positive. Time to wait for in-flight requests; afterwards their contexts are cancelled and connections closed. |
| `SHUTDOWN_DRAIN_DELAY` | `3s` | Not negative. After SIGTERM `/readyz` returns 503 for this long before the server stops accepting connections. |
| `TRUSTED_PROXIES` | empty | Comma-separated IPs/CIDRs of the reverse proxy (`172.28.0.10/32` in compose). Must be set behind nginx, otherwise every request appears to come from the proxy and the per-IP login limit is shared by all clients. Leave empty when the API is exposed directly. |
| `DATABASE_URL` | none | Required. |
| `DB_MAX_CONNS` | `20` | At least 2: migrations hold one connection for the advisory lock and need another. At least 3 when `RETENTION_ENABLED=true` because a retention job holds a connection for its advisory lock. |
| `DB_STATEMENT_TIMEOUT` | `30s` | Pool-wide `statement_timeout`. Not negative. |
| `DB_LOCK_TIMEOUT` | `5s` | `lock_timeout` for board row locks; on timeout the API answers `503 busy`. Not negative. |
| `MIGRATE_ON_START` | `true` | Run migrations on startup under an advisory lock. |
| `REDIS_URL` | none | Required unless `SINGLE_INSTANCE=true`. Backs pub/sub, presence, the login limiter and the REST limiter; `/readyz` also checks it. |
| `SINGLE_INSTANCE` | `false` | Development and single-replica opt-in: the login limiter, REST limiter, event pub/sub and presence run in process and `REDIS_URL` is not used. Never enable it with more than one API replica. |
| `JWT_SECRET` | none | Required, at least 32 bytes. |
| `JWT_ISSUER` | `cardo` | Must not be empty. |
| `JWT_AUDIENCE` | `cardo-api` | Must not be empty. |
| `ACCESS_TOKEN_TTL` | `15m` | Must be positive. |
| `REFRESH_TOKEN_TTL` | `168h` | Must be greater than `ACCESS_TOKEN_TTL`. |
| `LOGIN_MAX_FAILURES` | `5` | Failed logins per email and client IP within the window before `429 rate_limited`. Must be positive. |
| `LOGIN_FAILURE_WINDOW` | `1m` | Must be positive. |
| `ARGON2_MAX_CONCURRENT` | `0` (GOMAXPROCS) | Maximum concurrent password hashes; excess requests wait up to `ARGON2_ACQUIRE_TIMEOUT` and then get `503 overloaded`. Not negative. |
| `ARGON2_ACQUIRE_TIMEOUT` | `2s` | Must be positive. |
| `WS_ALLOWED_ORIGINS` | empty | Comma-separated allowlist of `Origin` values for WebSocket upgrades. Empty means same-origin only. Requests without an `Origin` header (native clients) are always accepted. |
| `METRICS_ADDR` | empty (disabled) | Listen address of the Prometheus listener, `:9091` in `.env.example` and compose. Must differ from `HTTP_ADDR`. |
| `RETENTION_ENABLED` | `true` | Turns the retention jobs on or off. |
| `RETENTION_INTERVAL` | `1h` | Time between runs. |
| `EVENTS_RETENTION` | `168h` | Age after which `board_events` may be pruned. Must not be shorter than `PROCESSED_OPS_RETENTION` (validated even when retention is disabled). |
| `EVENTS_KEEP_MIN` | `1000` | Newest events always kept per board. Not negative. |
| `PROCESSED_OPS_RETENTION` | `24h` | Age after which `processed_ops` rows are pruned. |
| `RETENTION_BATCH_SIZE` | `1000` | Rows per delete statement, between 1 and 50000. |
| `REST_RATE_LIMIT` | `20` | Sustained requests per second per user. `0` disables the limit; otherwise between `0.001` and `1000000`. |
| `REST_RATE_BURST` | `40` | Burst size, between 1 and 1000000 when the limit is enabled. |
| `LOG_LEVEL` | `info` | slog level: `debug`, `info`, `warn`, `error`. |

Used only by `docker-compose.yml`: `POSTGRES_PASSWORD` (default `cardo`) and `GRAFANA_ADMIN_PASSWORD` (required, set in `.env.example`).

## API overview

REST (`/api`): authentication, profile, boards, snapshot, event log, members, invites.
WebSocket (`/ws/boards/{id}`, `Authorization: Bearer <token>`): all board content mutations and all events.

The full machine-readable contract is [`docs/openapi.yaml`](docs/openapi.yaml) (OpenAPI 3.1): every route, request and response schema, the event payloads and the error codes. Open it in any Swagger UI or paste it into [editor.swagger.io](https://editor.swagger.io). A test (`internal/httpapi/openapi_test.go`) fails when the set of routes registered by the router and the set of operations in the spec differ, when an operation has no `operationId` or responses, or when a `$ref` does not resolve.

The WebSocket endpoint authenticates and checks board membership before upgrading. A missing or invalid token gets `401`; a user who is not a member of the board (or a board that does not exist) gets `404`, the same as the REST endpoints, so board existence is not leaked. A rejected `Origin` gets `403`; during shutdown the answer is `503 shutting_down`.

All REST errors use one body, `{"error": {"code": "...", "message": "..."}}`. Codes: `bad_request`, `validation_error`, `unauthorized`, `invalid_token`, `token_expired`, `token_reused`, `invalid_credentials`, `forbidden`, `not_found`, `method_not_allowed`, `email_taken`, `payload_too_large`, `rate_limited`, `not_ready`, `overloaded`, `busy`, `shutting_down`, `gone`, `already_member`, `invite_unavailable`, `internal`.

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/auth/register` | Register |
| POST | `/api/auth/login` | Token pair |
| POST | `/api/auth/refresh` | Rotate refresh token |
| POST | `/api/auth/logout` | Revoke refresh token |
| GET / PATCH | `/api/me` | Profile |
| GET / POST | `/api/boards` | List / create boards |
| GET / PATCH / DELETE | `/api/boards/{id}` | Snapshot / rename / soft delete |
| GET | `/api/boards/{id}/events?after_seq=N` | Event delta (`410 Gone` outside retention) |
| GET / POST | `/api/boards/{id}/members` | List / add members |
| PATCH / DELETE | `/api/boards/{id}/members/{userId}` | Change role / remove |
| GET / POST | `/api/boards/{id}/invites` | List active invites (owner, no tokens) / create invite |
| DELETE | `/api/boards/{id}/invites/{inviteId}` | Revoke invite |
| POST | `/api/invites/{token}/accept` | Accept invite |
| GET | `/ws/boards/{id}` | WebSocket upgrade |
| GET | `/healthz`, `/readyz` | Liveness / readiness |

Prometheus metrics are not part of the API listener; see [Metrics](#metrics).

### Members and invites

All member changes are made under the board lock and produce `member.added`, `member.role_changed` and `member.removed` events with a `seq`; they are published after commit and delivered over WebSocket like any other event. Live connections react immediately: a removed member is closed with `4003`, and a role change updates the cached role of that user's open connections.

- `GET /members` (any member): `user_id`, `display_name`, `avatar_color`, `role`, `joined_at`.
- `POST /members` (owner), body `{"email", "role": "editor|viewer"}`: adds an existing user. Unknown email is `404 not_found`, already a member is `409 already_member`. Non-owners get `403` (or `404` when not a member) before any email lookup, so they cannot probe which emails are registered.
- `PATCH /members/{userId}` (owner), body `{"role": "editor|viewer"}`: targeting the owner is `403 forbidden`, the same as for removal, because ownership transfer is not supported; setting the current role is a no-op without an event.
- `DELETE /members/{userId}`: the owner removes anyone except themselves; any other member may remove only themselves (leave). The owner cannot leave (`403`).
- `POST /invites` (owner), body `{"role": "editor|viewer", "expires_in": seconds (default 7 days, max 30 days), "max_uses": 1..100 (default 1)}`: returns `id`, `role`, `expires_at`, `max_uses`, `uses` and the raw `token` exactly once. The token is 256 bits of randomness, base64url; only its SHA-256 is stored.
- `POST /api/invites/{token}/accept` (any user): atomically checks the invite is not revoked, not expired, has uses left and that the board is not deleted, then consumes one use and adds the member (`member.added`). Unknown token is `404`; a revoked, expired or exhausted invite is `410 invite_unavailable`. A user who is already a member gets `200` with the board and their current role even when the invite has since been revoked, has expired or is exhausted: the membership check comes first, so no use is consumed and no event is emitted. The token travels in the path, so request logs record only the route template (`/api/invites/:token/accept`), and nginx does not log these paths.

## WebSocket protocol

Endpoint: `GET /ws/boards/{id}` with `Authorization: Bearer <access token>`. Messages are JSON text frames; binary frames are a protocol violation. The maximum message size is 64 KB.

### Envelope v1

```json
{"v": 1, "type": "card.moved", "op_id": "…", "seq": 42, "server_ts": 1767225600000, "payload": {}}
```

| Field | Meaning |
|-------|---------|
| `v` | Protocol version, always `1`. Other versions are a protocol violation. |
| `type` | Message type. |
| `op_id` | Client-generated UUID (required on operations). On events it is the `op_id` of the operation that produced the event; absent otherwise. |
| `seq` | Board-wide, strictly increasing event number. Only on events. |
| `server_ts` | Server time in Unix milliseconds. Only on events. |
| `payload` | Type-specific object. |

Client to server: `hello`, `auth.refresh` and operations. Server to client: `sync.delta`, `sync.snapshot`, `sync.ready`, `presence.list`, `presence.join`, `presence.leave`, `auth.refreshed`, `nack`, `error` and events.

### Handshake

```mermaid
sequenceDiagram
    participant C as Client
    participant S as Server
    C->>S: GET /ws/boards/{id} (Bearer token)
    S-->>C: 101 Switching Protocols
    C->>S: hello {last_seq, client}
    alt last_seq set and the gap is retained and at most 5000 events
        S-->>C: sync.delta {events} (chunks of at most 500)
    else no last_seq, 410 Gone, last_seq above the board seq or a larger gap
        S-->>C: sync.snapshot {board, columns, cards, members, seq}
    end
    S-->>C: presence.list {users}
    S-->>C: sync.ready
    Note over C,S: operations are accepted from sync.ready on
    C->>S: op.card.move {op_id, payload}
    S-->>C: card.moved {op_id, seq} (the acknowledgement)
    S-->>C: events of other clients (seq N+1, N+2 …)
```

- `hello` payload: `{"last_seq": <number or null>, "client": "<optional string>"}`. Omit `last_seq` (or send `null`) for a fresh start. A negative `last_seq` is a protocol violation. `hello` must arrive within 15 s and only once.
- Catch-up policy: with `last_seq` the server collects the events after it. If they are no longer retained (`410`), `last_seq` is above the board `seq`, or more than 5000 events are missing, it sends a `sync.snapshot` instead. The client replaces its confirmed state with the snapshot. This is counted by `resync_total{kind="delta|snapshot"}`.
- Live events that arrive while the handshake runs are delivered after the delta or snapshot and deduplicated by `seq`; the client still applies everything strictly by `seq`.
- Presence changes after the handshake arrive as `presence.join {user: {id, name, avatar_color}}` and `presence.leave {user_id}`.

### Operations, acknowledgements and nacks

Operations are sent after `sync.ready`. The payload is decoded strictly: unknown fields are rejected.

| Type | Payload | Notes |
|------|---------|-------|
| `op.card.create` | `card_id`, `column_id`, `title`, optional `description`, `after_id`, `before_id` | Client picks `card_id`. Title up to 500 characters, description up to 10000. |
| `op.card.edit` | `card_id`, optional `title`, `description`, `base_description_version` | Titles are last-writer-wins; description edits are checked against `base_description_version`. |
| `op.card.move` | `card_id`, `to_column_id`, optional `after_id`, `before_id`, `base_version` | Checked against `base_version`. |
| `op.card.archive` | `card_id` | |
| `op.column.create` | `column_id`, `title`, optional `after_id`, `before_id` | Title up to 200 characters. |
| `op.column.rename` | `column_id`, `title` | |
| `op.column.move` | `column_id`, optional `after_id`, `before_id`, `base_version` | |
| `op.column.archive` | `column_id` | Archives its cards too. |

- **Ack.** There is no separate ack message. The operation is acknowledged by the event that carries its `op_id` (the sender receives it like every other client). Replaying an already processed `op_id` returns the stored result: the same event again, or the same nack.
- **Events.** `card.created`, `card.edited`, `card.moved`, `card.archived`, `column.created`, `column.renamed`, `column.moved`, `column.archived`, `column.rebalanced`, `columns.rebalanced`, `board.renamed`, `board.deleted`, `member.added`, `member.role_changed`, `member.removed`. Rebalance events (new `rank` values after the fractional index ran out of room) carry no `op_id`. Payload schemas are in `docs/openapi.yaml`.
- **Nack.** `{"v":1,"type":"nack","op_id":"…","payload":{"reason","at_seq","message"}}` where `at_seq` is the board `seq` at rejection time.

| Nack reason | Meaning |
|-------------|---------|
| `version_conflict` | `base_version` or `base_description_version` is stale. The operation is not retried. |
| `not_found` | A referenced card or column does not exist (any more). |
| `forbidden` | The role does not allow changes (viewer). The server re-reads the role from the database before nacking. |
| `validation_error` | The payload failed validation. |
| `rate_limited` | The connection exceeded 20 operations per second (burst 20). |
| `resync_required` | The operation was already applied but its event has been pruned by retention, so the stored result cannot be replayed. See [Retention](#retention). |
| `internal` | Temporary server error. Retry with the same `op_id`. |

Per-connection operation rate limit: 20 operations per second with a burst of 20. Each excess operation gets a `rate_limited` nack; 100 of them within 10 s close the connection with `4008`.

`error` messages (`{"code","message"}`) report protocol problems: `protocol_error` for unknown types, malformed messages, an unsupported version, operations before `sync.ready`, a repeated `hello`; `invalid_token` for a bad `auth.refresh`; `internal` before a `1011` close. Two protocol violations close the connection with `4400`, as does a missing `hello` after 15 s.

### Token refresh and heartbeat

- The access token's expiry applies to the open socket: when it passes, the server closes with `4001`. Before that the client sends `auth.refresh {"access_token": "<new token>"}` and receives `auth.refreshed {"expires_at": <unix ms>}`. The new token must belong to the same user.
- The server pings every 30 s and expects a pong within 60 s; otherwise it closes with `1000`.
- Membership is revalidated on `auth.refresh`, on every 15 s heartbeat and after a `forbidden` nack, so a removed member is dropped with `4003` and a role change takes effect without reconnecting.

### Close codes

| Code | Meaning |
|------|---------|
| 1000 | Pong timeout |
| 1001 | Server shutting down (also when joining during shutdown) |
| 1009 | Message over 64 KB |
| 1011 | Sync failure during the handshake |
| 1013 | Join failure, try again later |
| 4001 | Token expired |
| 4003 | Access revoked or member removed |
| 4004 | Board deleted (or not found) |
| 4008 | Rate-limit abuse |
| 4009 | Slow client: send buffer overflow or handshake backlog flush timeout |
| 4400 | Protocol violation |

Reasonable client behaviour: refresh the token and reconnect after `4001`; reconnect with backoff after `1000`, `1001`, `1011`, `1013`, `4009` and abnormal closures; do not reconnect after `4003` and `4004`.

### Client algorithm

Clients keep a confirmed state (what the server has acknowledged) and a queue of pending operations applied optimistically on top of it:

1. **Confirmed.** Apply events strictly in `seq` order, only `seq == confirmed.seq + 1`. An event with a lower or equal `seq` is a duplicate; if it carries the `op_id` of a pending operation, that operation is resolved.
2. **Out-of-order buffer.** An event with a higher `seq` is buffered. If the gap does not close within 1 s, fetch `GET /api/boards/{id}/events?after_seq=<confirmed.seq>` (repeat while `has_more`). On `410 Gone`, reload `GET /api/boards/{id}` and replace the state.
3. **Pending.** Every local operation is added to the pending queue with its `op_id` and sent. It leaves the queue when the event with its `op_id` arrives or a nack arrives.
4. **Nack.** Remove the nacked operation and re-derive the view; pending operations that no longer apply to the rebased state (dependants such as a move of a card whose create was nacked) are dropped as well. `resync_required` is not a rejection: the operation was applied; drop it from pending, reload the snapshot and resend the remaining pending operations.
5. **Reconnect.** Reconnect, send `hello{last_seq: confirmed.seq}`, apply the delta or snapshot, rebase the pending operations on the new confirmed state, and resend them with their original `op_id` (the server deduplicates). Pending operations that were applied while offline come back as events with the same `op_id` and resolve themselves.

`internal/simclient` is a reference implementation of this algorithm used by the convergence tests.

## Metrics

Metrics are served as `GET /metrics` on `METRICS_ADDR` (`:9091` in compose), a listener separate from the API. nginx does not forward it; Prometheus scrapes each replica directly. With `METRICS_ADDR` empty, the endpoint and all instrumentation are off. Go runtime and process collectors are included.

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `ws_connections` | gauge | none | Currently open WebSocket connections. |
| `ws_closed_total` | counter | `code` | Closed or refused WebSocket connections by close code (`other` for unknown codes). |
| `resync_total` | counter | `kind` (`delta`, `snapshot`) | WebSocket handshakes by catch-up kind. |
| `ops_total` | counter | `type`, `result` | Processed board operations. `type` is the op type or `unknown`; `result` is `applied`, `replayed`, `canceled` or a nack reason (`version_conflict`, `not_found`, `forbidden`, `validation_error`, `rate_limited`, `resync_required`, `internal`). |
| `op_apply_duration_seconds` | histogram | `type` | Time to apply an operation (default buckets). |
| `event_publish_errors_total` | counter | none | Failed event publish batches to Redis. |
| `http_request_duration_seconds` | histogram | `method`, `route`, `status` | REST request duration by route template (`unmatched` for unknown routes). |
| `retention_deleted_total` | counter | `table` | Rows removed by the retention jobs. |
| `retention_runs_total` | counter | `table`, `result` (`ok`, `skipped`, `failed`, `interrupted`) | Retention job runs. |
| `retention_last_success_timestamp_seconds` | gauge | `table` | Unix time of the last successful run. |
| `rest_rate_limited_total` | counter | none | REST requests rejected by the per-user limit. |
| `rest_rate_limiter_errors_total` | counter | none | Limiter backend failures; those requests were allowed through. |

`table` is `board_events` or `processed_ops`.

## Retention

A background job in every replica (unless `RETENTION_ENABLED=false`) runs once at startup and then every `RETENTION_INTERVAL` (1 h):

- **`board_events`**: events older than `EVENTS_RETENTION` (7 days) are deleted, but each board always keeps its newest `EVENTS_KEEP_MIN` (1000) events. Pruning removes a contiguous prefix of a board's log, in batches of `RETENTION_BATCH_SIZE` rows, so the log never has holes. Boards that were soft-deleted are pruned completely once their events are old enough.
- **`processed_ops`** (idempotency records): rows older than `PROCESSED_OPS_RETENTION` (24 h) are deleted in batches.
- **One replica at a time.** Each job takes a Postgres advisory lock with `pg_try_advisory_lock` on a dedicated connection; a replica that does not get the lock skips the run (`retention_runs_total{result="skipped"}`). Short batches keep row locks brief, and the job never takes the board row lock used by operations.
- `EVENTS_RETENTION` must not be shorter than `PROCESSED_OPS_RETENTION`, so events normally outlive the idempotency records that point to them. `DB_MAX_CONNS` must be at least 3 when retention is enabled.

What clients see:

- **`GET /api/boards/{id}/events?after_seq=N`** answers `410 Gone` when the oldest retained event is newer than `N + 1`. The client reloads the snapshot.
- **WebSocket `hello{last_seq}`** with an old `last_seq` is answered with `sync.snapshot` instead of a delta.
- **`resync_required` nack.** A replayed operation re-reads its event from `board_events` by `seq`. If retention removed that event while the `processed_ops` row was still present, the server cannot reproduce the original answer and sends `nack{reason:"resync_required"}`. The operation was applied, so the client must not treat it as rejected: drop it from pending, reload the board through the snapshot, and continue. It is counted as `ops_total{result="resync_required"}`.

## REST rate limit

Every authenticated REST request is limited per user (the user ID from the access token) with a token bucket:

- Defaults: `REST_RATE_LIMIT=20` requests per second sustained, `REST_RATE_BURST=40`. `REST_RATE_LIMIT=0` disables the limit.
- Over the limit the API answers `429 rate_limited` with a `Retry-After` header (seconds) in the unified error body. It is counted in `rest_rate_limited_total`.
- Backend: Redis (GCRA algorithm in a Lua script, keys `ratelimit:rest:{user}`) so the limit is shared by all replicas; an in-process limiter when `SINGLE_INSTANCE=true`.
- Fail-open: if Redis errors or times out (250 ms), the request is allowed and `rest_rate_limiter_errors_total` is incremented (a warning is logged at most every 30 s). A circuit breaker opens after 5 consecutive failures and skips Redis for 5 s, so a Redis outage does not add latency to every request.
- Exempt: `/healthz`, `/readyz`, `/api/auth/register`, `/api/auth/login`, `/api/auth/refresh` (these have the login limiter instead: `LOGIN_MAX_FAILURES` per email and client IP) and the WebSocket upgrade. Everything else under `/api`, including `/api/auth/logout`, is limited. WebSocket operations have their own limit (20 ops/s per connection).

## Load test

`cmd/loadtest` registers users, builds a board, opens WebSocket clients through the base URL and measures send-to-receive latency and resync time.

```bash
docker compose up --build -d
go build -o loadtest ./cmd/loadtest
./loadtest -url http://localhost:8080
```

The defaults are the target scenario: 50 clients on one board, 20 operations per second in total, 5 s warm-up, 60 s measured, resync after 1000 missed events (3 trials). Flags:

| Flag | Default | Meaning |
|------|---------|---------|
| `-url` | `http://localhost:8080` | API or nginx base URL |
| `-clients` | `50` | WebSocket clients on one board |
| `-rate` | `20` | Total operations per second |
| `-duration` | `60s` | Measured phase |
| `-warmup` | `5s` | Unrecorded warm-up |
| `-resync-events` | `1000` | Events the board advances while a client is disconnected |
| `-resync-trials` | `3` | Resync measurements (`0` skips) |
| `-register-parallelism` | `4` | Concurrent registrations during setup |
| `-seed` | current time | Seed of the operation mix |
| `-p95-target` | `200ms` | Latency target |
| `-resync-target` | `2s` | Resync target |
| `-refresh-interval` | `5m` | Access token renewal interval (`0` disables); keep below `ACCESS_TOKEN_TTL` |
| `-password` | built-in | Password of the generated users |
| `-json` | off | Print the report as JSON |
| `-enforce` | off | Exit with status 2 when a target is missed |

Exit codes: `0` success, `1` error, `2` target missed with `-enforce`, `64` usage. The generated users and board persist; run `docker compose down -v` to reset.

Headline result on the full compose stack (two replicas behind nginx, one workstation): send-to-receive **p95 10 ms** (p99 25.6 ms, 60000 samples, target below 200 ms), no lost events, and a worst resync of **342 ms** after about 1000 missed events (target below 2 s). The environment, method and full output are in [`docs/loadtest-report.md`](docs/loadtest-report.md).

## Tests

```bash
go test -race ./...
```

Integration tests start PostgreSQL and Redis with testcontainers and need a running Docker daemon. Without Docker they are skipped, unless `CI=true` or `REQUIRE_DOCKER=1` is set, in which case they fail.

| Setting | Effect |
|---------|--------|
| `REQUIRE_DOCKER=1` | Fail instead of skip when Docker is unavailable. |
| `TEST_DATABASE_URL`, `TEST_REDIS_URL` | Use existing servers instead of containers. |
| `go test -short` | Skips the container-backed integration tests and shortens the convergence runs. |
| `CONVERGENCE_SEED` | Seed of the random operation mix of the convergence tests (logged on every run, so a failure can be replayed). |
| `CONVERGENCE_OPS` | Operations per simulated client in the convergence tests. |

Notable suites:

- `internal/convergence`: the main convergence test runs 5 simulated clients (`internal/simclient`, implementing the client algorithm above) on 2 API instances sharing PostgreSQL and Redis. Each client performs 200 random operations (1000 in total) with random connection drops; after quiescence every client's view must equal the database snapshot. It runs over real Redis and over the in-memory broker.
- Event-loss variants drop Redis messages selectively, including short blackouts; clients must detect the gaps by `seq`, fetch the events over REST and still converge. Further scenarios cover pruned events (`resync_required`) and a rebalance hot spot.
- `deploy`: tests that check the nginx configuration (no invite tokens in logs, `/metrics` hidden, WebSocket headers and timeouts, body limit, round-robin).
- `internal/httpapi/openapi_test.go`: keeps `docs/openapi.yaml` in sync with the router.

## Development workflow and CI

Branching follows **Git Flow**:

- `main` — stable, release-ready code
- `develop` — integration branch
- `feature/*` — new features, branched from `develop` and merged back into it
- `release/*`, `hotfix/*` — release preparation and urgent fixes

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat(auth): add refresh token rotation
fix(ws): close connection with 4003 on member removal
test(ops): add concurrent move conflict test
```

Code in this repository has no comments.

CI (GitHub Actions, `.github/workflows/ci.yml`) runs on pushes to `main`, `develop`, `feature/**`, `release/**` and `hotfix/**`, and on pull requests to `main` and `develop`:

1. **Lint**: `gofmt`, a `go mod tidy` diff check, `go vet` and `golangci-lint`.
2. **Test**: `go test -race -count=1 -timeout 15m` with a coverage profile (uploaded as an artifact) and a coverage summary.
3. **Build**: `go build ./...` and `docker build`.

When tests fail, the failing lines are published as workflow annotations, readable without logging in:

```
GET https://api.github.com/repos/Max252t/Cardo-backend/check-runs/{job_id}/annotations
```
