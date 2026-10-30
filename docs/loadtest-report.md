# Load test report

Targets from spec 4: send-to-receive latency p95 below 200 ms with 50 clients on one board at 20 operations per second, and a resync after missing up to 1000 events in under 2 s.

Both targets are met with a wide margin. All figures below come from `cmd/loadtest` against the full compose stack on a single Windows workstation.

## Environment

| Item | Value |
|------|-------|
| Date | 2026-10-30 |
| Host OS | Windows 10 Home 10.0.19045 |
| CPU | Intel Core i5-10400F @ 2.90 GHz, 6 cores, 12 threads |
| RAM | 32 GB (Docker Desktop VM limited to 16 GB, 12 vCPUs) |
| Docker | Docker Desktop 4.95.0 (engine 29.8.2, WSL2 backend, kernel 6.18.54.1-microsoft-standard-WSL2) |
| Go | go1.26.3 (windows/386 toolchain, binaries built with `GOARCH=amd64`) |
| Load generator | `cmd/loadtest`, running natively on the Windows host, outside Docker |

Other containers from unrelated projects were running on the same Docker Desktop during the test (a Postgres, an API and a MinIO stack), so the numbers are not from an idle machine.

## Topology

`docker compose up --build -d` from this repository:

- `nginx` 1.27 (round robin, no sticky sessions, WebSocket upgrade on `/ws/`) published on the host;
- `api-1` and `api-2`, the same image built from the `Dockerfile`, `SINGLE_INSTANCE=false`;
- `postgres:16-alpine` and `redis:7-alpine` (Redis pub/sub fan-out and presence);
- settings from `.env.example` apart from a generated `JWT_SECRET`: `REST_RATE_LIMIT=20`, `REST_RATE_BURST=40`, `DB_MAX_CONNS=20`, retention enabled, WebSocket limit of 20 operations per second per connection.

Host port 8080 was already taken by another project, so nginx was published on 18080 with a one-line compose override (`ports: !override ["18080:80"]` on the `nginx` service). Nothing else differs from the committed `docker-compose.yml`. The stack was torn down with `docker compose down -v` afterwards.

## How the tool measures

- It registers N users, creates one board as the first user and adds the others as editors over REST (paced to stay inside the REST rate limit), then opens N WebSocket connections through the base URL. Nginx spreads them over both replicas.
- Operations (`op.card.create`, `op.card.edit`, `op.card.move`) are issued at a steady total rate and rotated round-robin over all clients, so each client sends `rate / clients` operations per second, far below the 20 ops/s per-connection limit.
- Latency uses a side channel and leaves the wire format untouched. The sender stores `time.Now()` right before the write in a table keyed by `op_id`. The primary event of an operation carries the same `op_id` (rebalance events carry none), so every receiver, including the sender, records `receive time - send time` when the event arrives, taken immediately after the socket read and before JSON parsing. All clients share one process and one monotonic clock, so there is no clock skew. With 50 clients each operation yields 50 samples.
- A run first warms up (unrecorded), then measures. After the measured phase it waits until every operation has reached every receiver and reports any event that never arrived.
- Resync: a client is disconnected, the board is advanced by 1000 operations from the other clients (about 15 ops/s each), and the client reconnects with `hello{last_seq}`. The reported times are measured from the start of the dial: dial, first `sync.delta`, `sync.ready`, and "caught up", which is the moment the client has seen every event up to the board sequence observed before it reconnected. The run also records one full snapshot handshake (`hello` without `last_seq`) for reference.

## Command lines

```
docker compose -f docker-compose.yml -f compose.ports.yml up --build -d
go build -o loadtest ./cmd/loadtest
./loadtest -url http://localhost:18080
```

The second command uses the defaults: 50 clients, 20 ops/s, 60 s measured after 5 s warm-up, 1000 events missed per resync, 3 resync trials. The headroom run used `./loadtest -url http://localhost:18080 -rate 100 -duration 30s -resync-trials 1`. `-json` prints the report as JSON and `-enforce` exits with status 2 when a target is missed.

## Results: target scenario

`loadtest -url http://localhost:18080` (seed 1793340060885411500)

```
Setup
  register 50 users: 557 ms, board and members: 3294 ms, connect all clients: 124 ms
  handshake      n=50      min=    8.1  mean=   23.2  p50=   15.9  p95=   41.4  p99=   42.4  max=   42.4 ms

Send-to-receive latency (op sent by one client, event received by a client)
  all receivers  n=60000   min=    3.5  mean=    6.8  p50=    6.1  p95=   10.0  p99=   25.6  max=   30.6 ms
  peers only     n=58800   min=    3.5  mean=    6.8  p50=    6.1  p95=   10.0  p99=   25.6  max=   30.6 ms
  sender only    n=1200    min=    3.6  mean=    6.8  p50=    6.1  p95=   10.1  p99=   25.8  max=   29.7 ms
  target p95 < 200 ms: PASS

Operations
  sent=1200 nacked=0 fully delivered=1200 missing receiver events=0 send errors=0
  nacks: version_conflict=14

Resync after a disconnect (hello with last_seq)
  trial 1: client 1 missed 991 events -> delta with 991 events, dial 8 ms, first sync 24 ms, ready 342 ms, caught up 342 ms (true)
  trial 2: client 2 missed 996 events -> delta with 996 events, dial 7 ms, first sync 19 ms, ready 26 ms, caught up 26 ms (true)
  trial 3: client 3 missed 999 events -> delta with 999 events, dial 4 ms, first sync 16 ms, ready 24 ms, caught up 24 ms (true)
  full snapshot (hello without last_seq): ready 38 ms
  slowest resync 342 ms, target < 2000 ms: PASS
```

| Target | Result | Verdict |
|--------|--------|---------|
| p95 send-to-receive latency below 200 ms | 10.0 ms (p99 25.6 ms, max 30.6 ms, 60000 samples) | met |
| Resync after about 1000 missed events below 2 s | 342 ms worst, 24 to 26 ms once warm | met |
| No lost events | 1200 of 1200 operations reached all 50 receivers | met |

The 14 `version_conflict` nacks all belong to operations outside the measured phase (the warm-up and the resync fill, where up to 20 clients move cards concurrently from a slightly stale version): the measured phase itself had no nack. They are counted in the run total but not in the latency table.

## Results: headroom run

`loadtest -url http://localhost:18080 -rate 100 -duration 30s -resync-trials 1` (seed 1793340152062607800), five times the specified rate:

```
  all receivers  n=150000  min=    2.8  mean=    5.0  p50=    5.0  p95=    6.0  p99=    7.0  max=   22.5 ms
  sent=3000 nacked=0 fully delivered=3000 missing receiver events=0 send errors=0
  trial 1: client 1 missed 998 events -> delta with 998 events, dial 7 ms, first sync 17 ms, ready 24 ms, caught up 24 ms (true)
  full snapshot (hello without last_seq): ready 45 ms
```

Latency does not grow at 100 ops/s, which means the 20 ops/s target is far from the capacity of two replicas on one Postgres.

## Reading the numbers

- The first resync is slower (342 ms) than the next two (about 25 ms). The delta read of about 1000 events is cold on the first call: connection setup in the pool, plan caching and page cache. Even this cold value is six times under the limit.
- A delta of 1000 events is below `MaxDeltaEvents` (5000), so the server answers with `sync.delta` in two chunks of at most 500 events. Larger gaps fall back to a snapshot; a full snapshot handshake of the boards used here took 38 to 45 ms.
- Latencies in the 20 to 30 ms range at p99 are isolated spikes. The Windows clock source also quantises timestamps to roughly 0.5 to 1 ms steps, which is why the headroom run shows round values such as 5.0, 6.0 and 7.0 ms.

## Caveats

- Generator and system under test share one machine, so the numbers contain no real network latency beyond the Docker Desktop port forwarding and the WSL2 virtual network. They measure the cost of the server, Postgres commit, Redis pub/sub and the nginx hop.
- Docker Desktop on Windows adds a user-space forwarding layer for published ports and a virtualised network for the container-to-container traffic. On Linux with native Docker the same stack should be equal or faster: a lower and tighter p99, and no first-call penalty from the virtualised disk. Cross-host deployments will add the real round-trip time of the network between client and nginx to every figure; the server-side part measured here is the budget that is left after that.
- Access tokens live 15 minutes (`ACCESS_TOKEN_TTL`); the tool does not refresh them, so a single run must stay under that, which is plenty for the defaults.
- Every run creates new users and a new board. Run `docker compose down -v` to reset the data.
- No target was missed, so there is no deviation to explain.
