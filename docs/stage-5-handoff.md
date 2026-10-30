# Stage 5 handoff: deploy, observability, retention, load and convergence

Instructions for the agent continuing Cardo backend development on a machine with Docker.
Stages 1–4 are merged into `develop`. This document is everything needed to finish stage 5 and cut the first release.

## 1. Setup

```bash
git clone https://github.com/Max252t/Cardo-backend.git
cd Cardo-backend
git checkout develop
```

Required tools:

- Go 1.26+ (see `go.mod`)
- Docker with Compose v2 (testcontainers, `docker compose`)
- golangci-lint v2: `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`
- A C toolchain so that `go test -race` works (CI runs with `-race`)

Spec: the product spec is `kanban-tz-v2.md` (Russian), kept outside the repository. Ask the user for a copy. The parts relevant to stage 5 are summarised in section 5 below, so the work can start without it.

Sanity check before writing code:

```bash
go build ./...
go vet ./...
golangci-lint run ./...
go test -race -count=1 ./...
```

All integration tests use testcontainers (`internal/testutil`). With `CI=true` or `REQUIRE_DOCKER=1` they fail instead of skipping when Docker is unavailable. `TEST_DATABASE_URL` / `TEST_REDIS_URL` point them at existing servers instead of containers.

## 2. Where the project stands

| Stage | Content | State |
|-------|---------|-------|
| 1 | Config, migrations with advisory lock, auth (argon2id, JWT, refresh rotation with reuse detection, atomic login rate limit), profile, boards REST, health, graceful shutdown, Dockerfile | merged |
| 2 | Fractional `rank`, columns and cards, OpProcessor (board row lock, `processed_ops` idempotency, version conflicts, nacks), snapshot and event log endpoints | merged |
| 3 | WebSocket hub (handshake, rate limit, heartbeat, close codes), Redis pub/sub fan-out, Redis presence with atomic Lua join/leave | merged |
| 4 | Members and invites, `member.*` events, live 4003 and role updates with membership revalidation, single-owner index | merged |
| 5 | Deploy, metrics, retention jobs, REST rate limit, convergence test, load test, docs | **this document** |

### Architecture (Clean Architecture, enforced by depguard)

```
domain, rank, clock, secrets              innermost, stdlib + uuid only
boardcontent, boardtx, boardevents        inner shapes and ports
ops, boards, users, auth, presence, members   use cases
protocol                                  outer wire format: envelope v1, session messages, close codes, channel names
storage, pubsub, presencestore, ratelimit, jwtauth, argon2id   adapters
httpapi (Gin), realtime (gorilla)         delivery
cmd/api                                   the only composition root
realtime/rttest, testutil                 test support, never imported by production code
```

Rules:

- Dependencies point inward. Use cases depend only on inner packages and on ports they declare themselves.
- Adapters implement ports. Delivery layers only translate between transport and use cases.
- No global state. Inject the clock, ID generator and logger. There is no `slog.SetDefault`.
- `.golangci.yml` has three depguard rules: `inner-layer`, `use-cases` and `production-code`. A new inner or use-case package must be added to the matching rule. A new adapter must not be imported by use cases.
- `boardtx.Tx` is the content transaction used by `ops` and `boards`. `boardtx.AdminTx` adds roster and invites and is reachable only through `Store.WithAdminLock`.

## 3. Conventions

- **No code comments at all**: no `//` or `/* */` in Go, no SQL comments. The only allowed line is the `//go:embed` directive.
- **Commits** are English [Conventional Commits](https://www.conventionalcommits.org/), small and logical. Use `feat`, `fix`, `refactor`, `test`, `docs`, `build`, `ci`, `perf` or `chore`.
- **No `Co-Authored-By` or any AI attribution** in commits or PR descriptions.
- **Branching** is Git Flow: `feature/*` from `develop`, merged back with `git merge --no-ff` after green CI, then `release/*` → `main`.
- **CI** (`.github/workflows/ci.yml`) runs gofmt, a `go mod tidy` diff check, `go vet`, golangci-lint, `go test -race` with coverage, `go build` and `docker build`. It runs on `main`, `develop`, `feature/**`, `release/**` and `hotfix/**`. Failing tests are published as workflow annotations, readable without logging in:
  `GET https://api.github.com/repos/Max252t/Cardo-backend/check-runs/{job_id}/annotations`.
- **Before every commit** all of these must pass: `gofmt -l .` (empty), `go vet ./...`, `go build ./...`, `go mod tidy` (no diff), `go test ./...` and `golangci-lint run ./...`.
- **Logs** never contain tokens, passwords or card descriptions. Invite tokens travel in the URL path, so request logs record only the route template.

## 4. Agent workflow

The previous machine used this pipeline per feature branch. Keep it.

1. **Logic** (Sonnet) implements the feature with Clean Architecture and a few smoke tests.
2. **Tests** (Sonnet) writes the full suite. It does not change production code; it reports bugs and skips a test with `t.Skip("BUG: ...")` only when a bug blocks it.
3. **Review** (Opus) reports prioritised findings (blocker/major/minor/nit, with `file:line`, a failure scenario and a fix), refactoring ideas and a Clean Architecture check. It does not modify code.
4. **Logic** implements the review fixes and the refactoring ideas.
5. **Merge**: green CI, then `--no-ff` into `develop`.

There is no separate refactoring agent.

## 5. Stage 5 scope

Suggested split into three feature branches, each going through the full pipeline:

| Branch | Items |
|--------|-------|
| `feature/deploy-observability` | 5.1, 5.2, 5.7 |
| `feature/retention-ratelimit` | 5.3, 5.4, 5.5 |
| `feature/convergence-loadtest` | 5.6, 5.8 |

### 5.1 Docker Compose and nginx

- `docker-compose.yml` services:
  - `api-1` and `api-2`, built from the existing `Dockerfile`;
  - `nginx`;
  - `postgres:16-alpine` with a volume;
  - `redis:7-alpine`;
  - optionally `prometheus` and `grafana` behind a compose profile.
- Use healthchecks. The API waits for healthy Postgres and Redis. Migrations already run on startup under an advisory lock (`MIGRATE_ON_START=true`), so both API replicas can start at once.
- nginx in `deploy/nginx.conf`:
  - round-robin over both APIs with no sticky sessions;
  - WebSocket upgrade for `/ws/` with `Upgrade`/`Connection` headers and long `proxy_read_timeout`, more than the 30 s ping and 60 s pong window;
  - `X-Forwarded-For` / `X-Real-IP` forwarded;
  - `client_max_body_size` consistent with `HTTP_MAX_BODY_BYTES`.
- **Security nit from the stage 4 review:** the access log must not record invite tokens. Disable logging, or use a log format without the URI, for `location ~ ^/api/invites/.+/accept$`.
- API env for compose:
  - `TRUSTED_PROXIES` set to the compose network so the per-IP login limit sees real client IPs;
  - `SINGLE_INSTANCE=false`, `REDIS_URL` set;
  - `JWT_SECRET` from `.env`.
- `.env.example` already has compose-friendly hostnames (`postgres`, `redis`).
- **Acceptance:**
  - `docker compose up --build` gives a working stack;
  - register, log in, create a board and open two WebSocket clients routed to different replicas; each sees the other's moves;
  - `docker compose kill api-1`: its clients reconnect through nginx to `api-2` and converge to the database state (spec 3.5).

### 5.2 Metrics and health

- Use the Prometheus Go client and expose `GET /metrics`. The README already lists it, but it is **not implemented yet**.
- Required metrics (spec 14):
  - `ws_connections` (gauge);
  - `ops_total{type,result}` (counter; result is `applied` or the nack reason);
  - `op_apply_duration_seconds` (histogram);
  - `event_publish_errors_total` (counter);
  - `ws_closed_total{code}` (counter).
- Also useful: HTTP request duration by route template, and `resync_total{kind=delta|snapshot}` server-side for handshakes.
- Keep the architecture:
  - define small metric ports (recorders) in the consumers (`ops`, `realtime`, `boardevents` publishing, `httpapi`);
  - implement them in a Prometheus adapter package;
  - wire them in `cmd/api`;
  - use a no-op default so tests need no registry.
- Decide whether `/metrics` is exposed through nginx. Recommended: not publicly; scrape the replicas directly on the compose network.
- `/healthz` and `/readyz` exist already. `/readyz` checks Postgres and Redis and returns 503 while draining.

### 5.3 Retention jobs (spec 6)

- `board_events`: keep 7 days, but never fewer than the last 1000 events per board.
- `processed_ops`: keep 24 hours.
- Run hourly. With several replicas, only one may run a job at a time. Use a Postgres advisory lock (`pg_try_advisory_lock`) or a Redis lock. Delete in bounded batches so board locks are not starved.
- **Constraints from the stage 2 review:**
  - Replaying a stored outcome re-reads its event from `board_events` by `seq`. If that event was pruned, today it surfaces as an internal error wrapping `domain.ErrNotFound`, which is indistinguishable from "board not found" (WebSocket close 4004). Give it its own error and mapping.
  - Keep events at least as long as `processed_ops` (true with 7 days vs 24 hours, but assert it in config validation).
- The event log endpoint already answers `410 Gone` when `after_seq` is older than the oldest retained event. Add an integration test that pruning makes it fire, and that a WebSocket `hello` with an old `last_seq` falls back to a snapshot.
- Make intervals and windows configurable via env with spec defaults.

### 5.4 Per-user REST rate limit (spec 12)

- Spec 12 requires rate limits for login (done), REST per user (**missing**) and WS ops (done, 20 ops/s per connection).
- Add a Redis-backed per-user limiter for authenticated REST routes, with an in-memory version for `SINGLE_INSTANCE`. Reuse the `ratelimit` adapter style; the port belongs to the consumer.
- Over the limit, respond `429 rate_limited` with `Retry-After`, in the unified error format.
- This also mitigates the email probing noted in the stage 4 review: any user can create a board and probe emails through `POST /members`.
- Pick sensible defaults (for example 20 req/s burst 40 per user) and make them configurable.

### 5.5 Rank read optimisation (deferred from the stage 2 review)

- Today every card create or move reads all active ranks of the target column under the board lock, which is up to 2000 rows at 20 ops/s.
- Read only the anchor rows and their immediate neighbours, for example `WHERE rank > $after ORDER BY rank LIMIT 1`.
- Load the full list only when a rebalance is needed.
- Same for columns. Existing ops tests must stay green, and a benchmark should show the improvement.

### 5.6 Convergence and event-loss tests (spec 13)

- **Main convergence test:**
  - 5 simulated clients on 2 API instances perform 1000 random operations with random connection drops;
  - after quiescence, every client's view equals the database snapshot.
- Simulated clients must implement the client algorithm from spec 10.3:
  - `confirmed` state;
  - `pending` queue;
  - strict `seq` ordering with an out-of-order buffer (wait up to 1 s, then `GET /events?after_seq=`);
  - reconnect handshake, rebase, resend with the same `op_id`, dropping nacked ops and their dependants.
- **Event-loss test:** selectively drop Redis messages (the in-memory broker has `SetDropFilter`; for real Redis, a proxy or a broker wrapper). Clients must detect gaps and converge.
- Can run against real Postgres and Redis via testcontainers, or two in-process hubs on one `pubsub.MemoryNetwork` with the real `OpProcessor`. Prefer real Redis for at least one variant.
- Keep the run time reasonable in CI (CI test timeout is 15 min); a build tag or `-short` gate for the long variant is acceptable.

### 5.7 Graceful shutdown check

Already implemented. Verify it under compose:

- on SIGTERM, `/readyz` returns 503 for `SHUTDOWN_DRAIN_DELAY`;
- WebSockets close with 1001 and clients reconnect to the other replica;
- in-flight transactions finish.

### 5.8 Load test (spec 4)

- New tool `cmd/loadtest` (Go): 50 WebSocket clients on one board, 20 operations/s total, through nginx to 2 replicas.
- Each operation carries `client_send_ts`. Receivers measure send-to-receive latency of the resulting event. The originating client generates the `op_id`; for timing, use a side channel keyed by `op_id` rather than changing the wire format.
- Report p50, p95 and p99. Target from spec 4: **p95 < 200 ms**.
- Also measure resync: after a disconnect that skips up to 1000 events, resync must complete in under 2 s.
- Commit the report to the repository, for example `docs/loadtest-report.md`, with environment details, the command line and results.

### 5.9 README and docs

- The **Project layout** section is outdated. Replace it with the real package list and the layer diagram from section 2.
- Document `docker compose` usage, the metrics list, the retention settings, the REST rate limit and the load test command.
- Add diagrams: the handshake sequence and the op → event → fan-out flow.
- The CI section should mention `feature/**`, `release/**` and `hotfix/**` triggers and the failure annotations.

## 6. Behaviour worth knowing

- **WebSocket close codes:**

  | Code | Meaning |
  |------|---------|
  | 1000 | pong timeout |
  | 1001 | shutdown |
  | 1009 | message over 64 KB |
  | 1011 | sync failure |
  | 1013 | join failure |
  | 4001 | token expired |
  | 4003 | access revoked or member removed |
  | 4004 | board deleted |
  | 4008 | rate-limit abuse |
  | 4009 | slow client or backlog flush timeout |
  | 4400 | protocol violation |

- **Handshake:** `hello{last_seq}` → `sync.delta` or `sync.snapshot` → `presence.list` → `sync.ready`. Ops are accepted as soon as `sync.ready` is queued. Catch-up policy lives in `boards.CatchUp`.
- **Membership revalidation:** a live socket re-checks membership on `auth.refresh`, on every 15 s heartbeat and after a forbidden op. A cached role that denies an op is re-read from the database before nacking.
- **Presence:** Redis zset `presence:{board}` with members `user|instance`, a profile hash, and Lua scripts that do the check, the write and the `PUBLISH` atomically.
- **`SINGLE_INSTANCE=true`** runs the login limiter, pub/sub and presence in process. Never use it with more than one replica, including in compose.
- **`DB_MAX_CONNS`** must be at least 2. Migrations use their own small pool without `statement_timeout`.
- **Known Windows-only flake:** `TestMessageSizeLimitBoundary/limit` can see a connection reset instead of the 1009 close frame. It is stable on Linux CI.

## 7. Definition of done

- All three stage 5 branches merged into `develop` with green CI.
- `docker compose up --build` runs the full stack. A replica can be killed while clients converge.
- `/metrics` exposes the spec 14 metrics. Retention jobs and the REST rate limit are in place.
- The convergence and event-loss tests pass in CI.
- The load test report is committed, with p95 < 200 ms and resync under 2 s, or the deviation is explained.
- README is up to date.
- Then create `release/1.0.0` from `develop`, run the final checks, merge it into `main` and back into `develop`, and tag `v1.0.0`.
