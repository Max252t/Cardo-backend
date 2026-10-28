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
| `REDIS_URL` | — | Required unless `SINGLE_INSTANCE=true`. Backs pub/sub, presence and the login rate limiter; `/readyz` also checks it. |
| `SINGLE_INSTANCE` | `false` | Development and single-replica opt-in: the login rate limiter, event pub/sub and presence run in process and `REDIS_URL` is not used. Never enable it with more than one API replica. |
| `WS_ALLOWED_ORIGINS` | empty | Comma-separated allowlist of `Origin` values for WebSocket upgrades. Empty means same-origin only. Requests without an `Origin` header (native clients) are always accepted. |
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

The WebSocket endpoint authenticates and checks board membership before upgrading. A missing or invalid token gets `401`; a user who is not a member of the board (or a board that does not exist) gets `404`, the same as the REST endpoints, so board existence is not leaked. A rejected `Origin` gets `403`.

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
| GET / POST | `/api/boards/:id/invites` | List active invites (owner, no tokens) / create invite |
| DELETE | `/api/boards/:id/invites/:inviteId` | Revoke invite |
| POST | `/api/invites/:token/accept` | Accept invite |
| GET | `/healthz`, `/readyz` | Liveness / readiness |
| GET | `/metrics` | Prometheus metrics |

### Members and invites

All member changes are made under the board lock and produce `member.added`, `member.role_changed` and `member.removed` events with a `seq`; they are published after commit and delivered over WebSocket like any other event. Live connections react immediately: a removed member is closed with `4003`, and a role change updates the cached role of that user's open connections.

- `GET /members` (any member): `user_id`, `display_name`, `avatar_color`, `role`, `joined_at`.
- `POST /members` (owner), body `{"email", "role": "editor|viewer"}`: adds an existing user. Unknown email is `404 not_found`, already a member is `409 already_member`. Non-owners get `403` (or `404` when not a member) before any email lookup, so they cannot probe which emails are registered.
- `PATCH /members/:userId` (owner), body `{"role": "editor|viewer"}`: targeting the owner is `403 forbidden`, the same as for removal, because ownership transfer is not supported; setting the current role is a no-op without an event.
- `DELETE /members/:userId`: the owner removes anyone except themselves; any other member may remove only themselves (leave). The owner cannot leave (`403`).
- `POST /invites` (owner), body `{"role": "editor|viewer", "expires_in": seconds (default 7 days, max 30 days), "max_uses": 1..100 (default 1)}`: returns `id`, `role`, `expires_at`, `max_uses`, `uses` and the raw `token` exactly once. The token is 256 bits of randomness, base64url; only its SHA-256 is stored.
- `POST /api/invites/:token/accept` (any user): atomically checks the invite is not revoked, not expired, has uses left and that the board is not deleted, then consumes one use and adds the member (`member.added`). Unknown token is `404`; a revoked, expired or exhausted invite is `410 invite_unavailable`. A user who is already a member gets `200` with the board and their current role even when the invite has since been revoked, has expired or is exhausted: the membership check comes first, so no use is consumed and no event is emitted. The token travels in the path, so request logs record only the route template (`/api/invites/:token/accept`).

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
