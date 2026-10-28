# robotaxi-dispatch

A robotaxi dispatch backend in Go with an offline fleet simulator. The service takes ride requests over HTTP, writes them to Postgres with a transactional outbox, and matches them to vehicles in batches: Redis GEO for candidates, A* on a road network for ETAs, and the Hungarian algorithm for the assignment. A deterministic simulator compares batched assignment with greedy matching on the same demand.

## Results

Simulation, 5 seeds per configuration, mean ± sample std. Every strategy runs on the same demand and fleet start positions for a given seed, and vehicles always move along A* routes; the strategies differ only in the cost they decide with and the solver. Copied from [`docs/results.md`](docs/results.md), which `make bench` regenerates:

```
go run ./cmd/sim -vehicles 80 -rate 150 -peak 3 -minutes 120 -seeds 5 -batch 2s -sweep 40,60,80,100,120
```

**Strategy comparison, 80 vehicles** (150 requests/h off-peak, x3 in the middle third, 2 s batch window):

| strategy | requested | match rate | pickup wait p50 (s) | pickup wait p95 (s) | pickup wait avg (s) | time to match p95 (s) | empty km % | avg pickup km | A* cache hit rate | runtime (ms) |
|---|---|---|---|---|---|---|---|---|---|---|
| greedy-straight | 498 ± 30 | 0.983 ± 0.022 | 360 ± 75 | 786 ± 86 | 387 ± 64 | 218 ± 87 | 48.8 ± 2.2 | 3.61 ± 0.29 | 0.00 ± 0.00 | 111 ± 16 |
| greedy-road | 498 ± 30 | 0.986 ± 0.019 | 351 ± 75 | 775 ± 86 | 380 ± 60 | 210 ± 87 | 48.8 ± 2.0 | 3.61 ± 0.27 | 0.20 ± 0.02 | 479 ± 197 |
| hungarian-straight | 498 ± 30 | 0.985 ± 0.016 | 275 ± 28 | 628 ± 7 | 302 ± 21 | 136 ± 50 | 45.1 ± 0.9 | 3.10 ± 0.13 | 0.00 ± 0.00 | 102 ± 14 |
| hungarian-road | 498 ± 30 | 0.986 ± 0.014 | 265 ± 29 | 599 ± 25 | 292 ± 22 | 131 ± 52 | 44.4 ± 0.9 | 3.02 ± 0.15 | 0.21 ± 0.01 | 329 ± 95 |

**Fleet size sweep** (same demand and dispatch settings; only the fleet size changes):

| vehicles | strategy | match rate | pickup wait p50 (s) | pickup wait p95 (s) | time to match p95 (s) | empty km % |
|---|---|---|---|---|---|---|
| 40 | greedy-straight | 0.697 ± 0.047 | 476 ± 47 | 874 ± 30 | 295 ± 2 | 52.7 ± 1.6 |
| 40 | greedy-road | 0.703 ± 0.039 | 482 ± 64 | 867 ± 19 | 294 ± 1 | 52.7 ± 2.4 |
| 40 | hungarian-straight | 0.758 ± 0.032 | 322 ± 25 | 620 ± 25 | 242 ± 13 | 44.9 ± 1.5 |
| 40 | hungarian-road | 0.762 ± 0.036 | 322 ± 21 | 619 ± 31 | 239 ± 9 | 45.2 ± 1.6 |
| 60 | greedy-straight | 0.859 ± 0.042 | 413 ± 56 | 859 ± 27 | 291 ± 5 | 49.8 ± 1.4 |
| 60 | greedy-road | 0.859 ± 0.047 | 399 ± 55 | 863 ± 14 | 289 ± 7 | 49.9 ± 1.0 |
| 60 | hungarian-straight | 0.905 ± 0.027 | 284 ± 19 | 630 ± 22 | 223 ± 10 | 43.5 ± 0.7 |
| 60 | hungarian-road | 0.908 ± 0.029 | 284 ± 18 | 604 ± 19 | 222 ± 19 | 43.3 ± 0.8 |
| 80 | greedy-straight | 0.983 ± 0.022 | 360 ± 75 | 786 ± 86 | 218 ± 87 | 48.8 ± 2.2 |
| 80 | greedy-road | 0.986 ± 0.019 | 351 ± 75 | 775 ± 86 | 210 ± 87 | 48.8 ± 2.0 |
| 80 | hungarian-straight | 0.985 ± 0.016 | 275 ± 28 | 628 ± 7 | 136 ± 50 | 45.1 ± 0.9 |
| 80 | hungarian-road | 0.986 ± 0.014 | 265 ± 29 | 599 ± 25 | 131 ± 52 | 44.4 ± 0.9 |
| 100 | greedy-straight | 1.000 ± 0.000 | 239 ± 46 | 533 ± 73 | 6 ± 8 | 42.6 ± 3.6 |
| 100 | greedy-road | 1.000 ± 0.000 | 234 ± 43 | 529 ± 77 | 10 ± 16 | 42.6 ± 3.6 |
| 100 | hungarian-straight | 1.000 ± 0.000 | 236 ± 38 | 530 ± 68 | 7 ± 9 | 42.6 ± 3.2 |
| 100 | hungarian-road | 1.000 ± 0.000 | 233 ± 39 | 529 ± 74 | 8 ± 12 | 42.6 ± 3.4 |
| 120 | greedy-straight | 1.000 ± 0.000 | 210 ± 20 | 450 ± 48 | 2 ± 1 | 38.7 ± 2.1 |
| 120 | greedy-road | 1.000 ± 0.000 | 204 ± 21 | 427 ± 55 | 2 ± 1 | 38.2 ± 2.1 |
| 120 | hungarian-straight | 1.000 ± 0.000 | 209 ± 20 | 447 ± 47 | 2 ± 1 | 38.6 ± 2.1 |
| 120 | hungarian-road | 1.000 ± 0.000 | 201 ± 19 | 428 ± 51 | 3 ± 1 | 38.1 ± 1.8 |

What the numbers say:

- Batched Hungarian assignment helps when the fleet is short of vehicles. From 40 to 80 vehicles, hungarian-road cuts p95 pickup wait by 24 to 30% against greedy-straight (874 to 619 s at 40, 786 to 599 s at 80) and raises the match rate at 40 and 60 vehicles (0.697 to 0.762, 0.859 to 0.908).
- At 100 and 120 vehicles there is no meaningful difference. Time to match p95 is 10 s or less, so rides are matched within a few batch windows and there is little left for joint assignment to improve.
- Road-network ETA instead of straight-line ETA improves p95 pickup wait by at most 5.1% on this grid (and is 0.5% worse in one configuration), and costs 3.2 to 4.3 times the simulation runtime at 80 vehicles. The differences are within one standard deviation. A plausible reason is that detours on a regular grid track straight-line distance closely; this was not tested on a real street map.
- Microbenchmarks on the same machine: Hungarian solves a dense 200 x 200 matrix in 3.1 ms; one A* route on the default grid takes 0.37 ms vs 0.50 ms for Dijkstra (`docs/results.md`, microbenchmark table).

The simulator fails a ride after 300 s unassigned (the service uses 120 s). Not modeled: traffic or time-varying speeds, decision latency (accept or reject is instant), rider cancellations, idle-vehicle repositioning, battery and charging, positions between nodes, and real streets (the road network is a synthetic 62 x 71 grid over San Francisco with 3 speed classes and 5% of edges closed).

## Architecture

```
  rider / vehicle clients
           |
           v
  +-----------------+   one transaction: ride + outbox row   +----------------------+
  |  HTTP API (chi) | -------------------------------------> | Postgres             |
  |  idempotency    |   + idempotency key                     | rides, vehicles,     |
  |  heartbeats ----+--> Redis: vehicle hash + vehicles:geo   | outbox_events, ...   |
  +-----------------+                                         +----------------------+
           ^                                                        |
           | decision / pickup / complete                           | FOR UPDATE SKIP LOCKED
           |                                                        v
  +-----------------------------------------+            +----------------------+
  | batch dispatcher (2 s or 50 rides)      | <--------- | outbox publisher     |
  |  lock:ride -> GEOSEARCH top K           |  ride IDs  | (at-least-once)      |
  |  -> A* ETA matrix -> Hungarian          |  (dedupe)  +----------------------+
  |  -> lock:vehicle -> conditional UPDATE  |
  |  sweeper: 15 s decision, 120 s fail     |      Prometheus scrapes /metrics
  +-----------------------------------------+      (alerts in deploy/prometheus)

  +--------------------------------------------------------------+
  | offline simulator (cmd/sim): same geo + assign packages,      |
  | no Postgres or Redis, deterministic per seed                  |
  +--------------------------------------------------------------+
```

## Design decisions

| problem | choice | trade-off |
|---|---|---|
| Matching one ride at a time is myopic | Collect a batch window (2 s or 50 rides) and solve it jointly | Every ride waits up to one window before matching; the sweep shows the benefit only appears when vehicles are scarce |
| Optimal assignment is O(n^3) | Hungarian from scratch; fall back to greedy when either side of the matrix exceeds 200 | 3.1 ms at 200 x 200 on the benchmark machine; larger batches lose optimality instead of stalling the worker |
| Two workers must never give one vehicle two rides | Redis lock per ride and per vehicle, `WHERE version = $v` on the ride, `WHERE status = 'AVAILABLE'` on the vehicle, and a partial unique index on active rides | The vehicle lock stops two commits on one vehicle at the same instant but can expire or be lost; the version check catches stale reads; the unique index holds even if both fail. A test removes the Redis locks and the invariant still holds |
| A crash between commit and publish must not lose a ride | Transactional outbox, published at-least-once with `SKIP LOCKED`; the dispatcher deduplicates by ride ID and skips rides that are no longer waiting | Duplicates are possible by design, so every consumer is idempotent; startup re-enqueues up to 10,000 waiting rides |
| Heartbeats every 2 s from every vehicle | Redis updated on every heartbeat; Postgres written at most once per 5 s per vehicle unless the reported status differs from the last stored one | Postgres location can be up to 5 s stale; dispatch reads Redis. Cache status writes are version-checked so a slow heartbeat cannot undo a dispatch |
| A* needs a heuristic that keeps results optimal | Straight-line distance divided by the fastest road speed | Admissible and consistent because no edge is shorter than the straight line or faster than the top speed; the property test checks A* against Dijkstra on 200 pairs |

## Correctness guarantees

| guarantee | mechanism | test |
|---|---|---|
| A vehicle never has two active rides | Partial unique index + conditional updates + vehicle lock | `TestConcurrentWorkersNeverDoubleBookVehicles` (20 workers, 5 vehicles, 20 rides), `TestDatabaseGuardsWithoutRedisLocks`, load test SQL check |
| A ride never has two vehicles | One `vehicle_id` column, set only by the version-checked dispatch update and cleared under a row lock | Same tests: every busy vehicle maps to exactly one active ride |
| One idempotency key creates exactly one ride (within the 24 h key lifetime) | Key row inserted first in the ride transaction; concurrent duplicates wait on the primary key and replay the stored response | `TestConcurrentSameIdempotencyKeyCreatesOneRide` (50 goroutines), 422 on a different body |
| Every committed ride produces an outbox event | Ride and outbox row in one transaction; row marked published only after hand-off | `TestEveryCommittedRideProducesAnEvent`, `TestCrashBetweenHandoffAndMarkIsDeliveredOnceAfterDedupe` |
| Illegal state changes are rejected | State machine checked inside the transaction; HTTP 409 | `TestRideStateMachine`, `TestTripLifecycleRecyclesVehicle`, `TestTripEndpoints` |
| Reject or timeout returns ride and vehicle | Ride back to MATCHING, vehicle to AVAILABLE and the GEO index, vehicle excluded from that ride | `TestRejectReturnsRideAndVehicle`, `TestDecisionTimeoutReturnsRideAndVehicle` |
| Hungarian is optimal | Compared with brute force | 500 random matrices up to 7 x 7, rectangular and +inf cases |
| The simulator is deterministic and consistent | Seeded RNGs; accept draws hashed from (seed, ride, attempt) | Determinism, wait >= time to match, no double booking, completed + failed + open = requested |

## Run locally

Needs Go 1.22+, Docker, and k6 for the load test.

```
make up                 # api, postgres, redis, prometheus (migrations run on start)
make migrate            # apply migrations from the host instead
make seed               # register 200 vehicles (N=50 make seed for fewer)
make sim                # run the simulator with the default config
make bench              # microbenchmarks + simulator; rewrites docs/results.md
make test               # unit tests with the race detector
make test-integration   # needs Postgres and Redis (TEST_DATABASE_URL, TEST_REDIS_ADDR)
make loadtest           # k6 + invariant checks against the running stack (start from an empty database); rewrites docs/loadtest.md
```

Integration tests truncate the tables they use, so do not point them at a database a running API is using. The API is on `:8080`, Prometheus on `:9090`. Ride lifecycle over HTTP:

```
POST /v1/rides                      Idempotency-Key required, returns 202
POST /v1/vehicles/{id}/heartbeat    response includes ride_id when the vehicle has an offer
POST /v1/dispatch/{ride_id}/decision {"vehicle_id": "...", "decision": "ACCEPT" | "REJECT"}
POST /v1/rides/{id}/pickup          {"vehicle_id": "..."}
POST /v1/rides/{id}/complete        {"vehicle_id": "..."}
```

## Load test

From [`docs/loadtest.md`](docs/loadtest.md), run on 2026-10-28 with `make loadtest` on one machine (Intel Xeon 2.10 GHz, 2 CPUs, 7.8 GB RAM) that also ran the API, Postgres, Redis, and k6. Scenario: 200 vehicles heartbeating every 2 s; ride requests ramp from 10 to 100/s over 3 minutes; 5% of requests resend an earlier Idempotency-Key; vehicles accept 90% of offers and call pickup and complete on their next heartbeats.

| endpoint | p50 (ms) | p95 (ms) | p99 (ms) |
|---|---|---|---|
| POST /v1/rides | 2.1 | 32.9 | 81.8 |
| POST /v1/vehicles/{id}/heartbeat | 45.6 | 124.6 | 161.0 |
| POST /v1/dispatch/{ride_id}/decision | 54.9 | 112.7 | 141.6 |
| POST /v1/rides/{id}/pickup | 55.3 | 112.9 | 144.7 |
| POST /v1/rides/{id}/complete | 55.1 | 112.2 | 142.0 |

- 49,191 requests at 232 requests/s overall, 0.00% non-2xx responses.
- 487 idempotent resends, all returned the original ride.
- Invariant checks after the run: 0 vehicles with two active rides, 0 rides under two keys, rides = idempotency keys, every ride has an outbox event, busy vehicles match active rides.
- 5,703 trips completed, an average of 26.9 trips/s over the 212 s run, while ride requests ramped to 100/s. Demand outgrew the 200-vehicle fleet: 20 s after k6 finished, 3,406 rides were still waiting for a vehicle and 103 had failed after 120 s unmatched. These are unserved rides in the scenario, not request errors.
- The dispatcher counted 2,623 assignment conflicts, commits refused by a version check or the active-ride index. The counter does not record the cause; the likely one is two workers choosing the same vehicle from the GEO index. No invariant was broken, and the retries are listed under limitations.

## Project structure

```
cmd/api            HTTP service, outbox publisher, dispatcher (also `api migrate`)
cmd/sim            simulator CLI and Markdown report
internal/geo       locations, grid road network, A*, LRU route cache
internal/assign    cost matrix helpers, Hungarian, greedy
internal/sim       demand generator, simulation engine, metrics
internal/store     Postgres repository, migrations runner, ride state machine
internal/cache     Redis vehicle index and locks
internal/dispatch  batch dispatcher, decisions, timeouts
internal/outbox    outbox publisher
internal/api       handlers, validation, idempotency middleware
internal/metrics   Prometheus collectors
migrations/        SQL schema
scripts/           seed script, k6 scenario, invariant SQL, report generator
deploy/prometheus  scrape config and alert rules
docs/              design notes and generated results
```

## Limitations

- The road network is a synthetic grid. Road ETA did not beat straight-line ETA by much here; a real street graph with rivers, hills, and one-way streets may change that, but it was not tested.
- The dispatch queue is in process. A restart loses queued work until the startup scan re-enqueues waiting rides, and running several API instances would split the queue without coordination beyond the locks and database guards.
- The load test recorded 2,623 refused commits. Two dispatch workers draw candidates from the same GEO index, which probably explains most of them, but the metric has no per-cause label to confirm it. Re-checking the vehicle after taking its lock, or partitioning batches by area, should cut the retries; neither was tried.
- Vehicles learn about offers by polling their heartbeat; there is no push channel.
- The load test ran on one 2-CPU machine shared with the database and k6, so its latencies are not a capacity estimate for separate hosts.
- No auth, no rate limiting, and the simulator does not model rider cancellations, repositioning, or charging.
