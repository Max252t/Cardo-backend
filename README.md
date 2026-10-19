# Cardo Backend

Backend for **Cardo** — a collaborative real-time Kanban board (Trello-lite) with a Flutter mobile client.
Several users move and edit cards on the same board simultaneously and see each other's changes instantly.

## Highlights

- Real-time sync over WebSocket with strict event ordering (`seq`) and gap recovery from an event log
- Server-side serialization of board operations via `SELECT … FOR UPDATE` on the board row
- Optimistic concurrency: card/column moves checked by `version`, description edits by `description_version`, titles are last-writer-wins
- Fractional indexing (`rank`, base62) for ordering — no mass renumbering
- Idempotent operations: client-generated UUIDs and a `processed_ops` table
- RBAC (`owner` / `editor` / `viewer`) applied to live connections immediately
- Horizontal scaling without sticky sessions: Redis Pub/Sub for events, Redis presence with TTL
- JWT access tokens + rotating refresh tokens with reuse detection

## Tech stack

| Layer | Technology |
|-------|------------|
| Language | Go 1.26+ |
| HTTP | Gin |
| WebSocket | gorilla/websocket |
| Database | PostgreSQL 15+ (pgx) |
| Cache / Pub/Sub / presence | Redis 7+ (go-redis) |
| Auth | golang-jwt, argon2id |
| Migrations | golang-migrate |
| Observability | slog (JSON), Prometheus |
| Tests | testing, testcontainers-go |

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

## Project layout

```
cmd/
  api/            API server entrypoint
  loadtest/       WebSocket load-testing tool
internal/
  config/         env-based configuration
  auth/           passwords, JWT, refresh token rotation
  httpapi/        Gin router, handlers, middleware
  storage/        PostgreSQL repositories
  rank/           fractional indexing
  ops/            OpProcessor: operation pipeline
  realtime/       WebSocket hub, clients, protocol
  presence/       Redis presence
  pubsub/         Redis event fan-out
migrations/       SQL migrations
deploy/           nginx and infrastructure configs
```

## Getting started

### Prerequisites

- Go 1.26+
- Docker and Docker Compose

### Run with Docker Compose

```bash
cp .env.example .env
docker compose up --build
```

The stack starts two API instances behind nginx, plus PostgreSQL and Redis. Migrations run automatically on startup.

### Run locally

```bash
go run ./cmd/api
```

### Configuration

All settings come from environment variables; see `.env.example` for the full list and defaults. Notable ones:

| Variable | Default | Notes |
|----------|---------|-------|
| `DATABASE_URL` | — | Required. |
| `REDIS_URL` | — | Required unless `LOGIN_LIMITER=memory`. When set, `/readyz` also checks Redis. |
| `LOGIN_LIMITER` | `redis` | `memory` is a development-only opt-in: the counter is per instance, so limits are not shared between API replicas. |
| `JWT_SECRET` | — | Required, at least 32 bytes. |
| `TRUSTED_PROXIES` | empty | Comma-separated IPs/CIDRs of the reverse proxy (the nginx container network, e.g. `172.16.0.0/12`). Must be set behind nginx, otherwise every request appears to come from the proxy and the per-IP login limit is shared by all clients. Leave empty when the API is exposed directly. |
| `DB_MAX_CONNS` | `20` | At least 2: migrations hold one connection for the advisory lock and need another. |
| `DB_STATEMENT_TIMEOUT` | `30s` | Pool-wide `statement_timeout`. |
| `DB_LOCK_TIMEOUT` | `5s` | `lock_timeout` for board row locks; on timeout the API answers `503 busy`. |
| `ARGON2_MAX_CONCURRENT` | `0` (GOMAXPROCS) | Maximum concurrent password hashes; excess requests wait up to `ARGON2_ACQUIRE_TIMEOUT` and then get `503 overloaded`. |
| `SHUTDOWN_DRAIN_DELAY` | `3s` | After SIGTERM `/readyz` returns 503 for this long before the server stops accepting connections. |
| `SHUTDOWN_TIMEOUT` | `20s` | Time to wait for in-flight requests; afterwards their contexts are cancelled and connections closed. |

### Tests

```bash
go test -race ./...
```

Integration tests use testcontainers and require a running Docker daemon.

## API overview

REST (`/api`): authentication, profile, boards, snapshot, event log, members, invites.
WebSocket (`/ws/boards/{id}`, `Authorization: Bearer <token>`): all board content mutations and all events.

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/auth/register` | Register |
| POST | `/api/auth/login` | Token pair |
| POST | `/api/auth/refresh` | Rotate refresh token |
| POST | `/api/auth/logout` | Revoke refresh token |
| GET / PATCH | `/api/me` | Profile |
| GET / POST | `/api/boards` | List / create boards |
| GET / PATCH / DELETE | `/api/boards/:id` | Snapshot / rename / soft delete |
| GET | `/api/boards/:id/events?after_seq=N` | Event delta (`410 Gone` outside retention) |
| GET / POST | `/api/boards/:id/members` | List / add members |
| PATCH / DELETE | `/api/boards/:id/members/:userId` | Change role / remove |
| POST / DELETE | `/api/boards/:id/invites[/:inviteId]` | Create / revoke invite |
| POST | `/api/invites/:token/accept` | Accept invite |
| GET | `/healthz`, `/readyz` | Liveness / readiness |
| GET | `/metrics` | Prometheus metrics |

## Development workflow

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

CI (GitHub Actions) runs on pushes and pull requests to `main` and `develop`: gofmt, `go mod tidy` check, `go vet`, golangci-lint, tests with the race detector, and build.
