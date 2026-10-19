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
